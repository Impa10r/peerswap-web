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

	log.Println("GetTransaction: txid not found")
	return nil, errors.New("txid not found")
}

func GetTxConfirmations(client lnrpc.LightningClient, txid string) int32 {
	tx, err := getTransaction(client, txid)
	if err == nil {
		return tx.NumConfirmations
	}

	log.Println("GetTxConfirmations:", err)
	return 0
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
// return rawTx hex string, final output amount, error
func PrepareRawTxWithUtxos(utxos *[]string, addr string, amount int64, feeRate uint64, subtractFeeFromAmount bool) (string, int64, error) {
	ctx := context.Background()
	conn, err := lndConnection()
	if err != nil {
		return "", 0, err
	}
	cl := walletrpc.NewWalletKitClient(conn)

	outputs := make(map[string]uint64)
	finalAmount := amount

	if subtractFeeFromAmount {
		if len(*utxos) == 0 {
			return "", 0, errors.New("at least one unspent output must be selected to send funds without change")
		}
		// give no output address,
		// let fundpsbt change back to the wallet to find the fee amount
	} else {
		outputs[addr] = uint64(amount)
	}

	psbtBytes, err := fundPsbt(cl, utxos, outputs, feeRate)
	if err != nil {
		log.Println("FundPsbt:", err)
		return "", 0, err
	}

	if subtractFeeFromAmount {
		// replace output with correct address and amount
		psbtString := base64.StdEncoding.EncodeToString(psbtBytes)
		fee, err := bitcoin.GetFeeFromPsbt(psbtString)
		if err != nil {
			return "", 0, err
		}
		// reduce output amount by fee and small haircut to go around the LND bug
		for haircut := int64(0); haircut <= 100; haircut += 5 {
			err := releaseOutputs(cl, utxos)
			if err != nil {
				return "", 0, err
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
			return "", 0, err
		}
	}

	// sign psbt
	res2, err := cl.FinalizePsbt(ctx, &walletrpc.FinalizePsbtRequest{
		FundedPsbt: psbtBytes,
	})

	if err != nil {
		log.Println("FinalizePsbt:", err)
		releaseOutputs(cl, utxos)
		return "", 0, err
	}

	rawTx := res2.GetRawFinalTx()

	return hex.EncodeToString(rawTx), finalAmount, nil
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
		ChangeType:       1, //P2TR
	})

	if err != nil {
		return nil, err
	}

	return res.GetFundedPsbt(), nil
}

func RbfPegin(feeRate uint64) (string, int64, error) {
	client, cleanup, err := GetClient()
	if err != nil {
		return "", 0, err
	}
	defer cleanup()

	rawTx, err := GetRawTransaction(client, config.Config.PeginTxId)
	if err != nil {
		return "", 0, err
	}

	decodedTx, err := bitcoin.DecodeRawTransaction(rawTx)
	if err != nil {
		return "", 0, err
	}

	var utxos []string

	for _, input := range decodedTx.Vin {
		vin := input.TXID + ":" + strconv.FormatUint(uint64(input.Vout), 10)
		utxos = append(utxos, vin)
	}

	conn, err := lndConnection()
	if err != nil {
		return "", 0, err
	}
	cl := walletrpc.NewWalletKitClient(conn)

	err = releaseOutputs(cl, &utxos)
	if err != nil {
		return "", 0, err
	}

	return PrepareRawTxWithUtxos(
		&utxos,
		config.Config.PeginAddress,
		config.Config.PeginAmount,
		feeRate,
		len(decodedTx.Vout) == 1)
}

func PublishTransaction(rawTx string, label string) (string, error) {
	conn, err := lndConnection()
	if err != nil {
		return "", err
	}
	cl := walletrpc.NewWalletKitClient(conn)
	ctx := context.Background()

	tx, err := hex.DecodeString(rawTx)
	if err != nil {
		return "", err
	}

	// Deserialize the transaction to get the transaction hash.
	msgTx := &wire.MsgTx{}
	txReader := bytes.NewReader(tx)
	if err := msgTx.Deserialize(txReader); err != nil {
		return "", err
	}

	req := &walletrpc.Transaction{
		TxHex: tx,
		Label: label,
	}

	_, err = cl.PublishTransaction(ctx, req)
	if err != nil {
		log.Println("PublishTransaction:", err)
		return "", err
	}
	return msgTx.TxHash().String(), nil
}
