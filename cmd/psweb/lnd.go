package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

func lndConnection() *grpc.ClientConn {
	tlsCertPath := getPeerswapConfSetting("lnd.tlscertpath")
	macaroonPath := getPeerswapConfSetting("lnd.macaroonpath")
	host := getPeerswapConfSetting("lnd.host")

	tlsCreds, err := credentials.NewClientTLSFromFile(tlsCertPath, "")
	if err != nil {
		log.Println("lnd Cannot get node tls credentials", err)
		return nil
	}

	macaroonBytes, err := os.ReadFile(macaroonPath)
	if err != nil {
		log.Println("lnd Cannot read macaroon file", err)
		return nil
	}

	mac := &macaroon.Macaroon{}
	if err = mac.UnmarshalBinary(macaroonBytes); err != nil {
		log.Println("lnd Cannot unmarshal macaroon", err)
		return nil
	}

	macCred, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		log.Println("lnd Cannot wrap macaroon", err)
		return nil
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(tlsCreds),
		grpc.WithBlock(),
		grpc.WithPerRPCCredentials(macCred),
	}

	conn, err := grpc.Dial(host, opts...)
	if err != nil {
		fmt.Println("lnd Cannot dial to lnd", err)
		return nil
	}

	return conn
}

func lndSendCoins(addr string, amount int64, feeRate uint64, sweepall bool, label string) (string, error) {
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
		log.Println("lnd Cannot send coins:", err)
		return "", err
	}

	return resp.Txid, nil
}

func lndConfirmedWalletBalance() int64 {
	client := lnrpc.NewLightningClient(lndConnection())
	ctx := context.Background()
	resp, err := client.WalletBalance(ctx, &lnrpc.WalletBalanceRequest{})
	if err != nil {
		log.Println("lnd Cannot get wallet balance:", err)
		return 0
	}

	return resp.ConfirmedBalance
}

func lndListUnspent() []*lnrpc.Utxo {
	client := walletrpc.NewWalletKitClient(lndConnection())
	ctx := context.Background()

	resp, err := client.ListUnspent(ctx, &walletrpc.ListUnspentRequest{MinConfs: 0})
	if err != nil {
		log.Println("lnd Cannot list unspent:", err)
		return nil
	}

	return resp.Utxos
}

func lndGetTransactions() []*lnrpc.Transaction {
	client := lnrpc.NewLightningClient(lndConnection())
	ctx := context.Background()

	resp, err := client.GetTransactions(ctx, &lnrpc.GetTransactionsRequest{})
	if err != nil {
		log.Println("lnd GetTransactions:", err)
		return nil
	}

	return resp.Transactions
}

func lndNumConfirmations(txid string) int32 {
	txs := lndGetTransactions()
	for _, tx := range txs {
		if tx.TxHash == txid {
			return tx.NumConfirmations
		}
	}
	return 0
}

func lndGetAlias(nodeKey string) string {
	client := lnrpc.NewLightningClient(lndConnection())
	ctx := context.Background()

	nodeInfo, err := client.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{PubKey: nodeKey})
	if err != nil {
		return ""
	}

	return nodeInfo.Node.Alias
}
