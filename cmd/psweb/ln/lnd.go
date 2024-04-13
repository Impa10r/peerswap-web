//go:build !clnversion

package ln

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"peerswap-web/cmd/psweb/config"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

func lndConnection() *grpc.ClientConn {
	tlsCertPath := config.GetPeerswapLNDSetting("lnd.tlscertpath")
	macaroonPath := config.GetPeerswapLNDSetting("lnd.macaroonpath")
	host := config.GetPeerswapLNDSetting("lnd.host")

	tlsCreds, err := credentials.NewClientTLSFromFile(tlsCertPath, "")
	if err != nil {
		log.Println("lndConnection", err)
		return nil
	}

	macaroonBytes, err := os.ReadFile(macaroonPath)
	if err != nil {
		log.Println("lndConnection", err)
		return nil
	}

	mac := &macaroon.Macaroon{}
	if err = mac.UnmarshalBinary(macaroonBytes); err != nil {
		log.Println("lndConnection", err)
		return nil
	}

	macCred, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		log.Println("lndConnection", err)
		return nil
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(tlsCreds),
		grpc.WithBlock(),
		grpc.WithPerRPCCredentials(macCred),
	}

	conn, err := grpc.Dial(host, opts...)
	if err != nil {
		fmt.Println("lndConnection", err)
		return nil
	}

	return conn
}

func SendCoins(addr string, amount int64, feeRate uint64, sweepall bool, label string) (string, error) {
	client := lnrpc.NewLightningClient(lndConnection())
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

func ConfirmedWalletBalance() int64 {
	client := lnrpc.NewLightningClient(lndConnection())
	ctx := context.Background()
	resp, err := client.WalletBalance(ctx, &lnrpc.WalletBalanceRequest{})
	if err != nil {
		log.Println("WalletBalance:", err)
		return 0
	}

	return resp.ConfirmedBalance
}

func ListUnspent(list *[]UTXO) error {
	client := walletrpc.NewWalletKitClient(lndConnection())
	ctx := context.Background()

	resp, err := client.ListUnspent(ctx, &walletrpc.ListUnspentRequest{MinConfs: 0})
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

	return nil
}

func GetTransaction(txid string) (*lnrpc.Transaction, error) {
	client := lnrpc.NewLightningClient(lndConnection())
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

func GetTxConfirmations(txid string) int32 {
	tx, err := GetTransaction(txid)
	if err == nil {
		return tx.NumConfirmations
	}

	log.Println("GetTxConfirmations:", err)
	return 0
}

func GetAlias(nodeKey string) string {
	client := lnrpc.NewLightningClient(lndConnection())
	ctx := context.Background()

	nodeInfo, err := client.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{PubKey: nodeKey})
	if err != nil {
		return ""
	}

	return nodeInfo.Node.Alias
}

func BumpFee(TxId string, outputIndex uint32, newFeeRate uint64) error {
	client := walletrpc.NewWalletKitClient(lndConnection())
	ctx := context.Background()

	_, err := client.BumpFee(ctx, &walletrpc.BumpFeeRequest{
		Outpoint: &lnrpc.OutPoint{
			TxidStr:     TxId,
			OutputIndex: outputIndex,
		},
		SatPerVbyte: newFeeRate,
	})

	if err != nil {
		log.Println("BumpFee:", err)
		return err
	}

	return nil
}
