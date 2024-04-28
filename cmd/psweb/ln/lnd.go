//go:build !cln

package ln

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"peerswap-web/cmd/psweb/bitcoin"
	"peerswap-web/cmd/psweb/config"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

const Implementation = "LND"

var LndVerson = float64(0) // 0.18+ for RBF ability

var internalLockId = []byte{
	0xed, 0xe1, 0x9a, 0x92, 0xed, 0x32, 0x1a, 0x47,
	0x05, 0xf8, 0xa1, 0xcc, 0xcc, 0x1d, 0x4f, 0x61,
	0x82, 0x54, 0x5d, 0x4b, 0xb4, 0xfa, 0xe0, 0x8b,
	0xd5, 0x93, 0x78, 0x31, 0xb7, 0xe3, 0x8f, 0x98,
}

func lndConnection() (*grpc.ClientConn, error) {
	tlsCertPath := config.GetPeerswapLNDSetting("lnd.tlscertpath")
	macaroonPath := config.GetPeerswapLNDSetting("lnd.macaroonpath")
	host := config.GetPeerswapLNDSetting("lnd.host")

	tlsCreds, err := credentials.NewClientTLSFromFile(tlsCertPath, "")
	if err != nil {
		log.Println("lndConnection", err)
		return nil, err
	}

	macaroonBytes, err := os.ReadFile(macaroonPath)
	if err != nil {
		log.Println("lndConnection", err)
		return nil, err
	}

	mac := &macaroon.Macaroon{}
	if err = mac.UnmarshalBinary(macaroonBytes); err != nil {
		log.Println("lndConnection", err)
		return nil, err
	}

	macCred, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		log.Println("lndConnection", err)
		return nil, err
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(tlsCreds),
		grpc.WithBlock(),
		grpc.WithPerRPCCredentials(macCred),
	}

	conn, err := grpc.Dial(host, opts...)
	if err != nil {
		fmt.Println("lndConnection", err)
		return nil, err
	}

	return conn, nil
}

func GetClient() (lnrpc.LightningClient, func(), error) {
	conn, err := lndConnection()
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() { conn.Close() }

	return lnrpc.NewLightningClient(conn), cleanup, nil
}

func ConfirmedWalletBalance(client lnrpc.LightningClient) int64 {
	ctx := context.Background()
	resp, err := client.WalletBalance(ctx, &lnrpc.WalletBalanceRequest{})
	if err != nil {
		log.Println("WalletBalance:", err)
		return 0
	}

	return resp.ConfirmedBalance
}

func ListUnspent(_ lnrpc.LightningClient, list *[]UTXO, minConfs int32) error {
	ctx := context.Background()
	conn, err := lndConnection()
	if err != nil {
		return err
	}
	cl := walletrpc.NewWalletKitClient(conn)
	resp, err := cl.ListUnspent(ctx, &walletrpc.ListUnspentRequest{MinConfs: minConfs})
	if err != nil {
		log.Println("ListUnspent:", err)
		return err
	}

	// Dereference the pointer to get the actual array
	a := *list

	// Append the value to the array
	for _, i := range resp.Utxos {
		a = append(a, UTXO{
			Address:       i.Address,
			AmountSat:     i.AmountSat,
			Confirmations: i.Confirmations,
			TxidBytes:     i.Outpoint.GetTxidBytes(),
			TxidStr:       i.Outpoint.GetTxidStr(),
			OutputIndex:   i.Outpoint.GetOutputIndex(),
		})
	}

	// Update the array through the pointer
	*list = a

	conn.Close()
	return nil
}

func getTransaction(client lnrpc.LightningClient, txid string) (*lnrpc.Transaction, error) {
	ctx := context.Background()
	resp, err := client.GetTransactions(ctx, &lnrpc.GetTransactionsRequest{})
	if err != nil {
		log.Println("GetTransaction:", err)
		return nil, err
	}
	for _, tx := range resp.Transactions {
		if tx.TxHash == txid {
			return tx, nil
		}
	}
	return nil, errors.New("txid not found")
}

// returns number of confirmations and whether the tx can be fee bumped
func GetTxConfirmations(client lnrpc.LightningClient, txid string) (int32, bool) {
	tx, err := getTransaction(client, txid)
	if err == nil {
		return tx.NumConfirmations, len(tx.OutputDetails) > 1
	}
	return -1, false // signal tx not found in local mempool
}

func GetAlias(nodeKey string) string {
	client, cleanup, err := GetClient()
	if err != nil {
		return ""
	}
	defer cleanup()

	ctx := context.Background()
	nodeInfo, err := client.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{PubKey: nodeKey})
	if err != nil {
		return ""
	}

	return nodeInfo.Node.Alias
}

func GetRawTransaction(client lnrpc.LightningClient, txid string) (string, error) {
	tx, err := getTransaction(client, txid)

	if err != nil {
		return "", err
	}

	return tx.RawTxHex, nil
}

// utxos: ["txid:index", ....]
func SendCoinsWithUtxos(utxos *[]string, addr string, amount int64, feeRate uint64, subtractFeeFromAmount bool) (*SentResult, error) {
	ctx := context.Background()
	conn, err := lndConnection()
	if err != nil {
		return nil, err
	}
	cl := walletrpc.NewWalletKitClient(conn)

	outputs := make(map[string]uint64)
	finalAmount := amount

	if subtractFeeFromAmount {
		if len(*utxos) == 0 {
			return nil, errors.New("at least one unspent output must be selected to send funds without change")
		}
		// give no output address,
		// let fundpsbt change back to the wallet to find the fee amount
	} else {
		outputs[addr] = uint64(amount)
	}

	psbtBytes, err := fundPsbt(cl, utxos, outputs, feeRate)
	if err != nil {
		log.Println("FundPsbt:", err)
		return nil, err
	}

	if subtractFeeFromAmount {
		// replace output with correct address and amount
		psbtString := base64.StdEncoding.EncodeToString(psbtBytes)
		fee, err := bitcoin.GetFeeFromPsbt(psbtString)
		if err != nil {
			return nil, err
		}
		// reduce output amount by fee and small haircut to go around the LND bug
		for haircut := int64(0); haircut <= 100; haircut += 5 {
			err = releaseOutputs(cl, utxos)
			if err != nil {
				return nil, err
			}

			finalAmount = int64(amount) - toSats(fee) - haircut
			if finalAmount < 0 {
				finalAmount = 0
			}
			outputs[addr] = uint64(finalAmount)
			psbtBytes, err = fundPsbt(cl, utxos, outputs, feeRate)
			if err == nil {
				break
			}
		}
		if err != nil {
			log.Println("FundPsbt:", err)
			releaseOutputs(cl, utxos)
			return nil, err
		}
	}

	// sign psbt
	res2, err := cl.FinalizePsbt(ctx, &walletrpc.FinalizePsbtRequest{
		FundedPsbt: psbtBytes,
	})

	if err != nil {
		log.Println("FinalizePsbt:", err)
		releaseOutputs(cl, utxos)
		return nil, err
	}

	rawTx := res2.GetRawFinalTx()

	// Deserialize the transaction to get the transaction hash.
	msgTx := &wire.MsgTx{}
	txReader := bytes.NewReader(rawTx)
	if err := msgTx.Deserialize(txReader); err != nil {
		log.Println("Deserialize:", err)
		return nil, err
	}

	req := &walletrpc.Transaction{
		TxHex: rawTx,
		Label: "Liquid Peg-in",
	}

	_, err = cl.PublishTransaction(ctx, req)
	if err != nil {
		log.Println("PublishTransaction:", err)
		releaseOutputs(cl, utxos)
		return nil, err
	}

	result := SentResult{
		RawHex:    hex.EncodeToString(rawTx),
		TxId:      msgTx.TxHash().String(),
		AmountSat: finalAmount,
	}

	return &result, nil
}

func releaseOutputs(cl walletrpc.WalletKitClient, utxos *[]string) error {
	ctx := context.Background()
	for _, i := range *utxos {
		parts := strings.Split(i, ":")

		index, err := strconv.Atoi(parts[1])
		if err != nil {
			log.Println("releaseOutputs:", err)
			break
		}

		_, err = cl.ReleaseOutput(ctx, &walletrpc.ReleaseOutputRequest{
			Id: internalLockId,
			Outpoint: &lnrpc.OutPoint{
				TxidStr:     parts[0],
				OutputIndex: uint32(index),
			},
		})

		if err != nil {
			log.Println("ReleaseOutput:", err)
			return err
		}
	}
	return nil
}

func fundPsbt(cl walletrpc.WalletKitClient, utxos *[]string, outputs map[string]uint64, feeRate uint64) ([]byte, error) {
	ctx := context.Background()
	var inputs []*lnrpc.OutPoint

	for _, i := range *utxos {
		parts := strings.Split(i, ":")

		index, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, err
		}

		inputs = append(inputs, &lnrpc.OutPoint{
			TxidStr:     parts[0],
			OutputIndex: uint32(index),
		})
	}

	res, err := cl.FundPsbt(ctx, &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_Raw{
			Raw: &walletrpc.TxTemplate{
				Inputs:  inputs,
				Outputs: outputs,
			},
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: feeRate,
		},
		MinConfs:         int32(1),
		SpendUnconfirmed: false,
		//ChangeType:       1, //P2TR in 0.17.5
	})

	if err != nil {
		return nil, err
	}

	return res.GetFundedPsbt(), nil
}

func BumpPeginFee(feeRate uint64) (*SentResult, error) {

	client, cleanup, err := GetClient()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	tx, err := getTransaction(client, config.Config.PeginTxId)
	if err != nil {
		return nil, err
	}

	conn, err := lndConnection()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	cl := walletrpc.NewWalletKitClient(conn)

	if !CanRBF() {
		err = doCPFP(cl, tx.GetOutputDetails(), feeRate)
		if err != nil {
			return nil, err
		} else {
			return &SentResult{
				TxId:      config.Config.PeginTxId,
				RawHex:    "",
				AmountSat: config.Config.PeginAmount,
			}, nil
		}
	}

	ctx := context.Background()
	res, err := cl.RemoveTransaction(ctx, &walletrpc.GetTransactionRequest{
		Txid: config.Config.PeginTxId,
	})
	if err != nil {
		log.Println("RemoveTransaction:", err)
		return nil, err
	}

	if res.Status != "Successfully removed transaction" {
		log.Println("RemoveTransaction:", res.Status)
		return nil, errors.New("cannot remove the previous transaction: " + res.Status)
	}

	var utxos []string
	for _, input := range tx.PreviousOutpoints {
		utxos = append(utxos, input.Outpoint)
	}

	err = releaseOutputs(cl, &utxos)
	if err != nil {
		return nil, err
	}

	return SendCoinsWithUtxos(
		&utxos,
		config.Config.PeginAddress,
		config.Config.PeginAmount,
		feeRate,
		len(tx.OutputDetails) == 1)

}

func doCPFP(cl walletrpc.WalletKitClient, outputs []*lnrpc.OutputDetail, newFeeRate uint64) error {
	if len(outputs) == 1 {
		return errors.New("peg-in transaction has no change output, not possible to CPFP")
	}

	// find change output
	outputIndex := uint32(999) // bump will fail if output not found
	for _, output := range outputs {
		if output.GetAddress() != config.Config.PeginAddress {
			outputIndex = uint32(output.OutputIndex)
			break
		}
	}

	ctx := context.Background()
	_, err := cl.BumpFee(ctx, &walletrpc.BumpFeeRequest{
		Outpoint: &lnrpc.OutPoint{
			TxidStr:     config.Config.PeginTxId,
			OutputIndex: outputIndex,
		},
		SatPerVbyte: newFeeRate,
	})

	if err != nil {
		log.Println("CPFP:", err)
		return err
	}

	log.Println("CPFP successful")
	return nil
}

func CanRBF() bool {
	if LndVerson == 0 {

		// get lnd server version
		client, cleanup, err := GetClient()
		if err != nil {
			return false
		}
		defer cleanup()

		res, err := client.GetInfo(context.Background(), &lnrpc.GetInfoRequest{})
		if err != nil {
			return false
		}
		version := res.GetVersion()
		parts := strings.Split(version, ".")

		a, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return false
		}

		LndVerson = a

		b, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return false
		}

		LndVerson += b / 100
	}

	return LndVerson >= 0.18
}
