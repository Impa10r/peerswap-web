package main

import (
	"context"
	"errors"
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
		log.Println("lndSendCoins:", err)
		return "", err
	}

	return resp.Txid, nil
}

func lndConfirmedWalletBalance() int64 {
	client := lnrpc.NewLightningClient(lndConnection())
	ctx := context.Background()
	resp, err := client.WalletBalance(ctx, &lnrpc.WalletBalanceRequest{})
	if err != nil {
		log.Println("lndConfirmedWalletBalance:", err)
		return 0
	}

	return resp.ConfirmedBalance
}

func lndListUnspent() []*lnrpc.Utxo {
	client := walletrpc.NewWalletKitClient(lndConnection())
	ctx := context.Background()

	resp, err := client.ListUnspent(ctx, &walletrpc.ListUnspentRequest{MinConfs: 0})
	if err != nil {
		log.Println("lndListUnspent:", err)
		return nil
	}

	return resp.Utxos
}

func lndGetTransaction(txid string) (*lnrpc.Transaction, error) {
	client := lnrpc.NewLightningClient(lndConnection())
	ctx := context.Background()

	resp, err := client.GetTransactions(ctx, &lnrpc.GetTransactionsRequest{})
	if err != nil {
		log.Println("lndGetTransaction:", err)
		return nil, err
	}
	for _, tx := range resp.Transactions {
		if tx.TxHash == txid {
			return tx, nil
		}
	}

	log.Println("lndGetTransaction: txid not found")
	return nil, errors.New("txid not found")
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

func lndBumpFee(TxId string, outputIndex uint32, newFeeRate uint64) error {
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
		log.Println("lndBumpFee:", err)
		return err
	}

	return nil
}
