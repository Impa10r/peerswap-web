//go:build !cln

package ln

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"

	"peerswap-web/cmd/psweb/config"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

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

func SendCoins(client lnrpc.LightningClient, addr string, amount int64, feeRate uint64, sweepall bool, label string) (string, error) {
	ctx := context.Background()
	resp, err := client.SendCoins(ctx, &lnrpc.SendCoinsRequest{
		Addr:        addr,
		Amount:      amount,
		SatPerVbyte: feeRate,
		SendAll:     sweepall,
		Label:       label,
	})
	if err != nil {
		log.Println("SendCoins:", err)
		return "", err
	}

	return resp.Txid, nil
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

func ListUnspent(_ lnrpc.LightningClient, list *[]UTXO) error {
	ctx := context.Background()
	conn, err := lndConnection()
	if err != nil {
		return err
	}
	cl := walletrpc.NewWalletKitClient(conn)
	resp, err := cl.ListUnspent(ctx, &walletrpc.ListUnspentRequest{MinConfs: 0})
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

func BumpPeginFee(newFeeRate uint64) (string, error) {
	client, cleanup, err := GetClient()
	if err != nil {
		return "", err
	}
	defer cleanup()

	tx, err := getTransaction(client, config.Config.PeginTxId)
	if err != nil {
		return "", err
	}

	if len(tx.OutputDetails) == 1 {
		return "", errors.New("peg-in transaction has no change output, not possible to CPFP")
	}

	outputIndex := uint32(999) // will fail if output not found
	for _, output := range tx.OutputDetails {
		if output.Amount != config.Config.PeginAmount {
			outputIndex = uint32(output.OutputIndex)
			break
		}
	}

	ctx := context.Background()
	conn, err := lndConnection()
	if err != nil {
		return "", err
	}
	cl := walletrpc.NewWalletKitClient(conn)

	_, err = cl.BumpFee(ctx, &walletrpc.BumpFeeRequest{
		Outpoint: &lnrpc.OutPoint{
			TxidStr:     config.Config.PeginTxId,
			OutputIndex: outputIndex,
		},
		SatPerVbyte: newFeeRate,
	})

	if err != nil {
		log.Println("BumpFee:", err)
		return "", err
	}

	log.Println("Fee bump successful")
	conn.Close()
	return "", nil
}

func GetRawTransaction(client lnrpc.LightningClient, txid string) (string, error) {
	tx, err := getTransaction(client, txid)

	if err != nil {
		return "", err
	}

	return tx.RawTxHex, nil
}

func FindChildTx(client lnrpc.LightningClient) string {
	tx, err := getTransaction(client, config.Config.PeginTxId)
	if err != nil {
		return ""
	}

	if len(tx.OutputDetails) == 1 {
		return ""
	}

	outputIndex := uint32(999) // will fail if output not found
	for _, output := range tx.OutputDetails {
		if output.Amount != config.Config.PeginAmount {
			outputIndex = uint32(output.OutputIndex)
			break
		}
	}

	ctx := context.Background()

	// find the child tx in the wallet
	resp, err := client.GetTransactions(ctx, &lnrpc.GetTransactionsRequest{})
	if err != nil {
		return ""
	}

	txid := ""
	timestamp := int64(0)
	vin := config.Config.PeginTxId + ":" + strconv.FormatUint(uint64(outputIndex), 10)
	for _, tx := range resp.Transactions {
		for _, in := range tx.PreviousOutpoints {
			// find the latest child spending our output
			if in.Outpoint == vin && tx.TimeStamp > timestamp {
				txid = tx.TxHash
				timestamp = tx.TimeStamp
			}
		}
	}

	return txid
}
