//go:build !cln

package ln

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"peerswap-web/cmd/psweb/bitcoin"
	"peerswap-web/cmd/psweb/config"

	"github.com/elementsproject/peerswap/peerswaprpc"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/zpay32"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

const (
	Implementation = "LND"
	// Liquid balance to reserve in auto swaps
	// Min is 1000, but the swap would spend it all on fee
	SwapFeeReserve = uint64(2000)
)

type InflightHTLC struct {
	OutgoingChannelId uint64
	IncomingHtlcId    uint64
	OutgoingHtlcId    uint64
	forwardingEvent   *lnrpc.ForwardingEvent
}

var (
	LndVerson = float64(0) // must be 0.18+ for RBF ability

	// arrays mapped per channel
	forwardsIn   = make(map[uint64][]*lnrpc.ForwardingEvent)
	forwardsOut  = make(map[uint64][]*lnrpc.ForwardingEvent)
	paymentHtlcs = make(map[uint64][]*lnrpc.HTLCAttempt)
	invoiceHtlcs = make(map[uint64][]*lnrpc.InvoiceHTLC)

	// inflight HTLCs mapped per Incoming channel
	inflightHTLCs = make(map[uint64][]*InflightHTLC)

	// las index for invoice subscriptions
	lastInvoiceSettleIndex uint64

	// last timestamps for downloads
	lastforwardCreationTs uint64
	lastPaymentCreationTs int64

	// default lock id used by LND
	internalLockId = []byte{
		0xed, 0xe1, 0x9a, 0x92, 0xed, 0x32, 0x1a, 0x47,
		0x05, 0xf8, 0xa1, 0xcc, 0xcc, 0x1d, 0x4f, 0x61,
		0x82, 0x54, 0x5d, 0x4b, 0xb4, 0xfa, 0xe0, 0x8b,
		0xd5, 0x93, 0x78, 0x31, 0xb7, 0xe3, 0x8f, 0x98,
	}
	// custom lock id needed when funding manually constructed PSBT
	myLockId = []byte{
		0x00, 0xe1, 0x9a, 0x92, 0xed, 0x32, 0x1a, 0x47,
		0x05, 0xf8, 0xa1, 0xcc, 0xcc, 0x1d, 0x4f, 0x61,
		0x82, 0x54, 0x5d, 0x4b, 0xb4, 0xfa, 0xe0, 0x8b,
		0xd5, 0x93, 0x78, 0x31, 0xb7, 0xe3, 0x8f, 0x98,
	}
)

func lndConnection() (*grpc.ClientConn, error) {
	tlsCertPath := config.GetPeerswapLNDSetting("lnd.tlscertpath")
	macaroonPath := config.GetPeerswapLNDSetting("lnd.macaroonpath")
	host := config.GetPeerswapLNDSetting("lnd.host")

	tlsCreds, err := credentials.NewClientTLSFromFile(tlsCertPath, "")
	if err != nil {
		log.Println("Error reading tlsCert:", err)
		return nil, err
	}

	macaroonBytes, err := os.ReadFile(macaroonPath)
	if err != nil {
		log.Println("Error reading macaroon:", err)
		return nil, err
	}

	mac := &macaroon.Macaroon{}
	if err = mac.UnmarshalBinary(macaroonBytes); err != nil {
		log.Println("lndConnection UnmarshalBinary:", err)
		return nil, err
	}

	macCred, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		log.Println("lndConnection NewMacaroonCredential:", err)
		return nil, err
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(tlsCreds),
		grpc.WithBlock(),
		grpc.WithPerRPCCredentials(macCred),
	}

	conn, err := grpc.Dial(host, opts...)
	if err != nil {
		fmt.Println("lndConnection dial:", err)
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
		// trick for LND before 0.18:
		// give empty outputs,
		// let fundpsbt return change back to the wallet so we can find the fee amount
	} else {
		outputs[addr] = uint64(amount)
	}

	lockId := internalLockId
	var psbtBytes []byte

	if subtractFeeFromAmount && CanRBF() {
		// new template since for LND 0.18+
		// change lockID to custom and construct manual psbt
		lockId = myLockId
		psbtBytes, err = fundPsbtSpendAll(cl, utxos, addr, feeRate)
		if err != nil {
			return nil, err
		}
	} else {
		psbtBytes, err = fundPsbt(cl, utxos, outputs, feeRate)
		if err != nil {
			log.Println("FundPsbt:", err)
			return nil, err
		}

		if subtractFeeFromAmount {
			// trick for LND before 0.18
			// replace output with correct address and amount
			fee, err := bitcoin.GetFeeFromPsbt(&psbtBytes)
			if err != nil {
				return nil, err
			}
			// reduce output amount by fee and small haircut to go around the LND bug
			for haircut := int64(0); haircut <= 2000; haircut += 5 {
				err = releaseOutputs(cl, utxos, &lockId)
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
					// PFBT was funded successfully
					break
				}
			}
			if err != nil {
				log.Println("FundPsbt:", err)
				releaseOutputs(cl, utxos, &lockId)
				return nil, err
			}
		}
	}

	// sign psbt
	res2, err := cl.FinalizePsbt(ctx, &walletrpc.FinalizePsbtRequest{
		FundedPsbt: psbtBytes,
	})
	if err != nil {
		log.Println("FinalizePsbt:", err)
		releaseOutputs(cl, utxos, &lockId)
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
		releaseOutputs(cl, utxos, &lockId)
		return nil, err
	}

	// confirm the final amount sent
	if subtractFeeFromAmount {
		finalAmount = msgTx.TxOut[0].Value
	}

	result := SentResult{
		RawHex:    hex.EncodeToString(rawTx),
		TxId:      msgTx.TxHash().String(),
		AmountSat: finalAmount,
	}

	return &result, nil
}

func releaseOutputs(cl walletrpc.WalletKitClient, utxos *[]string, lockId *[]byte) error {
	ctx := context.Background()
	for _, i := range *utxos {
		parts := strings.Split(i, ":")

		index, err := strconv.Atoi(parts[1])
		if err != nil {
			log.Println("releaseOutputs:", err)
			break
		}

		_, err = cl.ReleaseOutput(ctx, &walletrpc.ReleaseOutputRequest{
			Id: *lockId,
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
		ChangeType:       walletrpc.ChangeAddressType_CHANGE_ADDRESS_TYPE_P2TR,
	})

	if err != nil {
		return nil, err
	}

	return res.GetFundedPsbt(), nil
}

// manual construction of PSBT in LND 0.18+ to spend exact UTXOs with no change
func fundPsbtSpendAll(cl walletrpc.WalletKitClient, utxoStrings *[]string, address string, feeRate uint64) ([]byte, error) {
	ctx := context.Background()

	unspent, err := cl.ListUnspent(ctx, &walletrpc.ListUnspentRequest{
		MinConfs: 1,
	})
	if err != nil {
		log.Println("ListUnspent:", err)
		return nil, err
	}

	tx := wire.NewMsgTx(2)

	for _, utxo := range unspent.Utxos {
		for _, u := range *utxoStrings {
			parts := strings.Split(u, ":")
			index, err := strconv.Atoi(parts[1])
			if err != nil {
				return nil, err
			}

			if utxo.Outpoint.TxidStr == parts[0] && utxo.Outpoint.OutputIndex == uint32(index) {
				hash, err := chainhash.NewHash(utxo.Outpoint.TxidBytes)
				if err != nil {
					log.Println("NewHash:", err)
					return nil, err
				}
				_, err = cl.LeaseOutput(ctx, &walletrpc.LeaseOutputRequest{
					Id:                myLockId,
					Outpoint:          utxo.Outpoint,
					ExpirationSeconds: uint64(10),
				})
				if err != nil {
					log.Println("LeaseOutput:", err)
					return nil, err
				}

				tx.TxIn = append(tx.TxIn, &wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Hash:  *hash,
						Index: utxo.Outpoint.OutputIndex,
					},
				})
			}
		}
	}

	var harnessNetParams = &chaincfg.TestNet3Params
	if config.Config.Chain == "mainnet" {
		harnessNetParams = &chaincfg.MainNetParams
	}

	parsed, err := btcutil.DecodeAddress(address, harnessNetParams)
	if err != nil {
		log.Println("DecodeAddress:", err)
		return nil, err
	}

	pkScript, err := txscript.PayToAddrScript(parsed)
	if err != nil {
		log.Println("PayToAddrScript:", err)
		return nil, err
	}

	tx.TxOut = append(tx.TxOut, &wire.TxOut{
		PkScript: pkScript,
		Value:    int64(1), // this will be identified as change output to spend all
	})

	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		log.Println("NewFromUnsignedTx:", err)
		return nil, err
	}

	var buf bytes.Buffer
	_ = packet.Serialize(&buf)

	cs := &walletrpc.PsbtCoinSelect{
		Psbt: buf.Bytes(),
		ChangeOutput: &walletrpc.PsbtCoinSelect_ExistingOutputIndex{
			ExistingOutputIndex: 0,
		},
	}

	fundResp, err := cl.FundPsbt(ctx, &walletrpc.FundPsbtRequest{
		Template: &walletrpc.FundPsbtRequest_CoinSelect{
			CoinSelect: cs,
		},
		Fees: &walletrpc.FundPsbtRequest_SatPerVbyte{
			SatPerVbyte: feeRate,
		},
	})
	if err != nil {
		log.Println("fundPsbtSpendAll:", err)
		log.Println("PSBT:", base64.StdEncoding.EncodeToString(cs.Psbt))
		releaseOutputs(cl, utxoStrings, &myLockId)
		return nil, err
	}

	return fundResp.FundedPsbt, nil
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

	// sometimes remove transaction is not enough
	releaseOutputs(cl, &utxos, &internalLockId)
	releaseOutputs(cl, &utxos, &myLockId)

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

func GetMyAlias() string {
	if myNodeAlias == "" {
		client, cleanup, err := GetClient()
		if err != nil {
			return ""
		}
		defer cleanup()

		res, err := client.GetInfo(context.Background(), &lnrpc.GetInfoRequest{})
		if err != nil {
			return ""
		}
		myNodeAlias = res.GetAlias()
	}
	return myNodeAlias
}

func downloadForwards(client lnrpc.LightningClient) {
	// only go back 6 months
	start := uint64(time.Now().AddDate(0, -6, 0).Unix())

	// incremental download if substription was interrupted
	if lastforwardCreationTs > 0 {
		// continue from the last timestamp in seconds
		start = lastforwardCreationTs + 1
	}

	// download forwards
	offset := uint32(0)
	for {
		res, err := client.ForwardingHistory(context.Background(), &lnrpc.ForwardingHistoryRequest{
			StartTime:       start,
			IndexOffset:     offset,
			PeerAliasLookup: false,
			NumMaxEvents:    50000,
		})
		if err != nil {
			return
		}

		// sort by in and out channels
		for _, event := range res.ForwardingEvents {
			forwardsIn[event.ChanIdIn] = append(forwardsIn[event.ChanIdIn], event)
			forwardsOut[event.ChanIdOut] = append(forwardsOut[event.ChanIdOut], event)
		}

		n := len(res.ForwardingEvents)
		if n > 0 {
			// store the last timestamp
			lastforwardCreationTs = res.ForwardingEvents[n-1].TimestampNs / 1_000_000_000
		}
		if n < 50000 {
			// all events retrieved
			break
		}
		// next pull start from the next index
		offset = res.LastOffsetIndex
	}
}

func downloadPayments(client lnrpc.LightningClient) {
	// only go back 6 months
	start := uint64(time.Now().AddDate(0, -6, 0).Unix())

	// incremental download if substription was interrupted
	if lastPaymentCreationTs > 0 {
		// continue from the last timestamp in seconds
		start = uint64(lastPaymentCreationTs + 1)
	}

	offset := uint64(0)
	for {
		res, err := client.ListPayments(context.Background(), &lnrpc.ListPaymentsRequest{
			CreationDateStart: start,
			IncludeIncomplete: false,
			Reversed:          false,
			IndexOffset:       offset,
			MaxPayments:       50000,
		})
		if err != nil {
			return
		}

		// only append settled ones
		for _, payment := range res.Payments {
			appendPayment(payment)
		}

		n := len(res.Payments)
		if n > 0 {
			// store the last timestamp
			lastPaymentCreationTs = res.Payments[n-1].CreationTimeNs / 1_000_000_000
		}
		if n < 50000 {
			// all events retrieved
			break
		}

		// next pull start from the next index
		offset = res.LastIndexOffset
	}
}

func appendPayment(payment *lnrpc.Payment) {
	if payment == nil {
		return
	}

	if payment.Status == lnrpc.Payment_SUCCEEDED {
		if payment.PaymentRequest != "" {
			// Decode the payment request
			var harnessNetParams = &chaincfg.TestNet3Params
			if config.Config.Chain == "mainnet" {
				harnessNetParams = &chaincfg.MainNetParams
			}
			invoice, err := zpay32.Decode(payment.PaymentRequest, harnessNetParams)
			if err == nil {
				if invoice.Description != nil {
					if len(*invoice.Description) > 7 {
						if (*invoice.Description)[:8] == "peerswap" {
							// find swap id
							parts := strings.Split(*invoice.Description, " ")
							if parts[2] == "fee" && len(parts[4]) > 0 {
								// save rebate payment
								SwapRebates[parts[4]] = int64(payment.ValueMsat) / 1000
							}
							// skip peerswap-related payments
							return
						}
					}
				}
			}
		}
		for _, htlc := range payment.Htlcs {
			if htlc.Status == lnrpc.HTLCAttempt_SUCCEEDED {
				// get channel from the first hop
				chanId := htlc.Route.Hops[0].ChanId
				paymentHtlcs[chanId] = append(paymentHtlcs[chanId], htlc)
			}
		}
		// store the last timestamp
		lastPaymentCreationTs = payment.CreationTimeNs / 1_000_000_000
	}
}

func subscribePayments(ctx context.Context, client routerrpc.RouterClient) error {
	req := &routerrpc.TrackPaymentsRequest{NoInflightUpdates: false}
	stream, err := client.TrackPayments(ctx, req)
	if err != nil {
		return err
	}

	log.Println("Subscribed to payments")

	for {
		payment, err := stream.Recv()
		if err != nil {
			return err
		}

		appendPayment(payment)
	}
}

func subscribeForwards(ctx context.Context, client routerrpc.RouterClient) error {
	req := &routerrpc.SubscribeHtlcEventsRequest{}
	stream, err := client.SubscribeHtlcEvents(ctx, req)
	if err != nil {
		return err
	}

	log.Println("Subscribed to forwards")

	for {
		htlcEvent, err := stream.Recv()
		if err != nil {
			return err
		}

		if htlcEvent.EventType != routerrpc.HtlcEvent_FORWARD {
			continue
		}

		switch event := htlcEvent.Event.(type) {
		case *routerrpc.HtlcEvent_ForwardEvent:
			// add htlc to inflight queue until Settle event
			info := event.ForwardEvent.GetInfo()
			if info != nil {
				htlc := new(InflightHTLC)
				if htlcEvent.EventType == routerrpc.HtlcEvent_FORWARD {
					forwardingEvent := new(lnrpc.ForwardingEvent)

					forwardingEvent.FeeMsat = info.IncomingAmtMsat - info.OutgoingAmtMsat
					forwardingEvent.AmtInMsat = info.IncomingAmtMsat
					forwardingEvent.AmtOutMsat = info.OutgoingAmtMsat
					forwardingEvent.AmtIn = info.IncomingAmtMsat / 1000
					forwardingEvent.AmtOut = info.OutgoingAmtMsat / 1000
					forwardingEvent.TimestampNs = htlcEvent.TimestampNs

					htlc.forwardingEvent = forwardingEvent

					htlc.OutgoingChannelId = htlcEvent.OutgoingChannelId
					htlc.IncomingHtlcId = htlcEvent.IncomingHtlcId
					htlc.OutgoingHtlcId = htlcEvent.OutgoingHtlcId

					inflightHTLCs[htlcEvent.IncomingChannelId] = append(inflightHTLCs[htlcEvent.IncomingChannelId], htlc)
				}
			}
		case *routerrpc.HtlcEvent_ForwardFailEvent:
			// delete from queue
			removeInflightHTLC(htlcEvent.IncomingChannelId, htlcEvent.IncomingHtlcId)

		case *routerrpc.HtlcEvent_SettleEvent:
			// find HTLC in queue
			for _, htlc := range inflightHTLCs[htlcEvent.IncomingChannelId] {
				if htlc.IncomingHtlcId == htlcEvent.IncomingHtlcId {
					// add our stored forwards
					forwardsIn[htlcEvent.IncomingChannelId] = append(forwardsIn[htlcEvent.IncomingChannelId], htlc.forwardingEvent)
					// settled htlcEvent has no Outgoing info, take from queue
					forwardsOut[htlc.OutgoingChannelId] = append(forwardsOut[htlc.OutgoingChannelId], htlc.forwardingEvent)
					// store the last timestamp
					lastforwardCreationTs = htlc.forwardingEvent.TimestampNs / 1_000_000_000
					// delete from queue
					removeInflightHTLC(htlcEvent.IncomingChannelId, htlcEvent.IncomingHtlcId)
					break
				}
			}
		}
	}
}

// Function to remove an InflightHTLC object from a slice in the map by IncomingChannelId
func removeInflightHTLC(incomingChannelId, incomingHtlcId uint64) {
	// Retrieve the slice from the map
	htlcSlice, exists := inflightHTLCs[incomingChannelId]
	if !exists {
		return
	}

	// Find the index of the object with the given IncomingHtlcId
	index := -1
	for i, htlc := range htlcSlice {
		if htlc.IncomingHtlcId == incomingHtlcId {
			index = i
			break
		}
	}

	// If the object is found, remove it from the slice
	if index != -1 {
		inflightHTLCs[incomingChannelId] = append(htlcSlice[:index], htlcSlice[index+1:]...)
	}

	// If the slice becomes empty after removal, delete the map entry
	if len(inflightHTLCs[incomingChannelId]) == 0 {
		delete(inflightHTLCs, incomingChannelId)
	}
}

// cache all and subscribe to lnd
func SubscribeAll() {
	// loop forever until connected
	for {
		conn, err := lndConnection()
		if err != nil {
			continue
		}
		defer conn.Close()

		client := lnrpc.NewLightningClient(conn)
		ctx := context.Background()

		// only go back 6 months for itinial download
		start := uint64(time.Now().AddDate(0, -6, 0).Unix())
		offset := uint64(0)
		for {
			res, err := client.ListInvoices(ctx, &lnrpc.ListInvoiceRequest{
				CreationDateStart: start,
				Reversed:          false,
				IndexOffset:       offset,
				NumMaxInvoices:    50000,
			})
			if err != nil {
				return
			}

			for _, invoice := range res.Invoices {
				appendInvoice(invoice)
			}

			n := len(res.Invoices)
			if n > 0 {
				// global settle index for subscription
				lastInvoiceSettleIndex = res.Invoices[n-1].SettleIndex
			}
			if n < 50000 {
				// all invoices retrieved
				break
			}

			// next pull start from the next index
			offset = res.LastIndexOffset
		}

		// initial download forwards
		downloadForwards(client)
		routerClient := routerrpc.NewRouterClient(conn)

		go func() {
			// subscribe to Forwards
			for {
				if err := subscribeForwards(ctx, routerClient); err != nil {
					log.Println("Forwards subscription error:", err)
					time.Sleep(60 * time.Second)
					// incremental download from last timestamp + 1
					downloadForwards(client)
				}
			}
		}()

		// initial download payments
		downloadPayments(client)

		go func() {
			// subscribe to Payments
			for {
				if err := subscribePayments(ctx, routerClient); err != nil {
					log.Println("Payments subscription error:", err)
					time.Sleep(60 * time.Second)
					// incremental download from last timestamp + 1
					downloadPayments(client)
				}
			}
		}()

		// subscribe to Invoices
		for {
			if err := subscribeInvoices(ctx, client); err != nil {
				log.Println("Invoices subscription error:", err)
				time.Sleep(60 * time.Second)
			}
		}
	}
}

func appendInvoice(invoice *lnrpc.Invoice) {
	if invoice == nil {
		// precaution
		return
	}
	// only append settled htlcs
	if invoice.State == lnrpc.Invoice_SETTLED {
		if len(invoice.Memo) > 7 && invoice.Memo[:8] == "peerswap" {
			// find swap id
			parts := strings.Split(invoice.Memo, " ")
			if parts[2] == "fee" && len(parts[4]) > 0 {
				// save rebate payment
				SwapRebates[parts[4]] = int64(invoice.AmtPaidMsat) / 1000
			}
		} else {
			// skip peerswap-related
			for _, htlc := range invoice.Htlcs {
				if htlc.State == lnrpc.InvoiceHTLCState_SETTLED {
					invoiceHtlcs[htlc.ChanId] = append(invoiceHtlcs[htlc.ChanId], htlc)
				}
			}
		}
	}
}

func subscribeInvoices(ctx context.Context, client lnrpc.LightningClient) error {
	req := &lnrpc.InvoiceSubscription{
		SettleIndex: lastInvoiceSettleIndex,
	}
	stream, err := client.SubscribeInvoices(ctx, req)
	if err != nil {
		return err
	}

	log.Println("Subscribed to invoices")

	for {
		invoice, err := stream.Recv()
		if err != nil {
			return err
		}
		appendInvoice(invoice)
		lastInvoiceSettleIndex = invoice.SettleIndex
	}
}

// get routing statistics for a channel
func GetForwardingStats(channelId uint64) *ForwardingStats {
	var (
		result          ForwardingStats
		feeMsat7d       uint64
		assistedMsat7d  uint64
		feeMsat30d      uint64
		assistedMsat30d uint64
		feeMsat6m       uint64
		assistedMsat6m  uint64
	)

	// historic timestamps in ns
	now := time.Now()
	timestamp7d := uint64(now.AddDate(0, 0, -7).Unix()) * 1_000_000_000
	timestamp30d := uint64(now.AddDate(0, 0, -30).Unix()) * 1_000_000_000
	timestamp6m := uint64(now.AddDate(0, -6, 0).Unix()) * 1_000_000_000

	for _, e := range forwardsOut[channelId] {
		if e.TimestampNs > timestamp6m {
			result.AmountOut6m += e.AmtOut
			feeMsat6m += e.FeeMsat
			if e.TimestampNs > timestamp30d {
				result.AmountOut30d += e.AmtOut
				feeMsat30d += e.FeeMsat
				if e.TimestampNs > timestamp7d {
					result.AmountOut7d += e.AmtOut
					feeMsat7d += e.FeeMsat
				}
			}
		}
	}

	for _, e := range forwardsIn[channelId] {
		if e.TimestampNs > timestamp6m {
			result.AmountIn6m += e.AmtIn
			assistedMsat6m += e.FeeMsat
			if e.TimestampNs > timestamp30d {
				result.AmountIn30d += e.AmtIn
				assistedMsat30d += e.FeeMsat
				if e.TimestampNs > timestamp7d {
					result.AmountIn7d += e.AmtIn
					assistedMsat7d += e.FeeMsat
				}
			}
		}
	}

	result.FeeSat7d = feeMsat7d / 1000
	result.AssistedFeeSat7d = assistedMsat7d / 1000
	result.FeeSat30d = feeMsat30d / 1000
	result.AssistedFeeSat30d = assistedMsat30d / 1000
	result.FeeSat6m = feeMsat6m / 1000
	result.AssistedFeeSat6m = assistedMsat6m / 1000

	if result.AmountOut7d > 0 {
		result.FeePPM7d = result.FeeSat7d * 1_000_000 / result.AmountOut7d
	}
	if result.AmountIn7d > 0 {
		result.AssistedPPM7d = result.AssistedFeeSat7d * 1_000_000 / result.AmountIn7d
	}
	if result.AmountOut30d > 0 {
		result.FeePPM30d = result.FeeSat30d * 1_000_000 / result.AmountOut30d
	}
	if result.AmountIn30d > 0 {
		result.AssistedPPM30d = result.AssistedFeeSat30d * 1_000_000 / result.AmountIn30d
	}
	if result.AmountOut6m > 0 {
		result.FeePPM6m = result.FeeSat6m * 1_000_000 / result.AmountOut6m
	}
	if result.AmountIn6m > 0 {
		result.AssistedPPM6m = result.AssistedFeeSat6m * 1_000_000 / result.AmountIn6m
	}

	return &result
}

// get fees on the channel
func GetChannelInfo(client lnrpc.LightningClient, channelId uint64, nodeId string) *ChanneInfo {
	info := new(ChanneInfo)

	res2, err := client.GetChanInfo(context.Background(), &lnrpc.ChanInfoRequest{
		ChanId: channelId,
	})
	if err != nil {
		log.Println("GetChanInfo:", err)
		return info
	}

	policy := res2.Node1Policy
	if res2.Node1Pub == nodeId {
		// the first policy is not ours, use the second
		policy = res2.Node2Policy
	}

	info.FeeRate = uint64(policy.GetFeeRateMilliMsat())
	info.FeeBase = uint64(policy.GetFeeBaseMsat())

	return info
}

// flow stats for a channel since timestamp
func GetChannelStats(channelId uint64, timeStamp uint64) *ChannelStats {

	var (
		result        ChannelStats
		routedInMsat  uint64
		routedOutMsat uint64
		feeMsat       uint64
		assistedMsat  uint64
		paidOutMsat   int64
		invoicedMsat  uint64
		costMsat      int64
	)

	timestampNs := timeStamp * 1_000_000_000

	for _, e := range forwardsOut[channelId] {
		if e.TimestampNs > timestampNs {
			routedOutMsat += e.AmtOutMsat
			feeMsat += e.FeeMsat
		}
	}
	for _, e := range forwardsIn[channelId] {
		if e.TimestampNs > timestampNs {
			routedInMsat += e.AmtInMsat
			assistedMsat += e.FeeMsat
		}
	}
	for _, e := range invoiceHtlcs[channelId] {
		if uint64(e.AcceptTime) > timeStamp {
			invoicedMsat += e.AmtMsat
		}
	}
	for _, e := range paymentHtlcs[channelId] {
		if uint64(e.AttemptTimeNs) > timestampNs {
			paidOutMsat += e.Route.TotalAmtMsat
			costMsat += e.Route.TotalFeesMsat
		}
	}

	result.RoutedOut = routedOutMsat / 1000
	result.RoutedIn = routedInMsat / 1000
	result.FeeSat = feeMsat / 1000
	result.AssistedFeeSat = assistedMsat / 1000
	result.InvoicedIn = invoicedMsat / 1000
	result.PaidOut = uint64(paidOutMsat / 1000)
	result.PaidCost = uint64(costMsat / 1000)

	return &result
}

// generate new onchain address
func NewAddress() (string, error) {
	ctx := context.Background()
	client, cleanup, err := GetClient()
	if err != nil {
		return "", err
	}
	defer cleanup()

	res, err := client.NewAddress(ctx, &lnrpc.NewAddressRequest{
		Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
	})
	if err != nil {
		log.Println("NewAddress:", err)
		return "", err
	}

	return res.Address, nil
}

func SendKeysendMessage(destPubkey string, amountSats int64, message string) error {
	ctx := context.Background()
	conn, err := lndConnection()
	if err != nil {
		return err
	}
	defer conn.Close()

	client := routerrpc.NewRouterClient(conn)

	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		return err
	}

	hash := sha256.Sum256(randomBytes)
	paymentHash := hex.EncodeToString(hash[:])
	paymentHashFinal, err := hex.DecodeString(paymentHash)
	if err != nil {
		return err
	}

	preImage := hex.EncodeToString(randomBytes)
	preImageFinal, err := hex.DecodeString(preImage)
	if err != nil {
		return err
	}

	pk, err := hex.DecodeString(destPubkey)
	if err != nil {
		return err
	}

	request := &routerrpc.SendPaymentRequest{
		Dest:        pk,
		Amt:         amountSats,
		PaymentHash: paymentHashFinal,
		DestCustomRecords: map[uint64][]byte{
			5482373484: preImageFinal,
			34349334:   []byte(message),
		},
		TimeoutSeconds: 60,
		FeeLimitSat:    10,
	}

	stream, err := client.SendPaymentV2(ctx, request)
	if err != nil {
		return fmt.Errorf("error sending kensend: %v", err)
	}

	for {
		resp, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("error receiving response: %v", err)
		}
		if resp.Status == lnrpc.Payment_SUCCEEDED {
			break
		}
	}
	return nil
}

// Returns Lightning channels as peerswaprpc.ListPeersResponse, excluding private channels and certain nodes
func ListPeers(client lnrpc.LightningClient, peerId string, excludeIds *[]string) (*peerswaprpc.ListPeersResponse, error) {
	ctx := context.Background()

	res, err := client.ListPeers(ctx, &lnrpc.ListPeersRequest{})
	if err != nil {
		return nil, err
	}

	var peers []*peerswaprpc.PeerSwapPeer

	for _, lndPeer := range res.Peers {
		// skip excluded
		if excludeIds != nil && stringIsInSlice(lndPeer.PubKey, *excludeIds) {
			continue
		}

		// skip if not the signle one requested
		if peerId != "" && lndPeer.PubKey != peerId {
			continue
		}

		bytePeer, err := hex.DecodeString(lndPeer.PubKey)
		if err != nil {
			return nil, err
		}

		res, err := client.ListChannels(ctx, &lnrpc.ListChannelsRequest{
			PublicOnly: true,
			Peer:       bytePeer,
		})
		if err != nil {
			return nil, err
		}

		// skip peers with no channels
		if len(res.Channels) == 0 {
			continue
		}

		peer := peerswaprpc.PeerSwapPeer{}
		peer.NodeId = lndPeer.PubKey

		for _, channel := range res.Channels {
			peer.Channels = append(peer.Channels, &peerswaprpc.PeerSwapPeerChannel{
				ChannelId:     channel.ChanId,
				LocalBalance:  uint64(channel.LocalBalance),
				RemoteBalance: uint64(channel.RemoteBalance),
				Active:        channel.Active,
			})
		}

		peer.AsSender = &peerswaprpc.SwapStats{}
		peer.AsReceiver = &peerswaprpc.SwapStats{}
		peers = append(peers, &peer)

		if peer.NodeId == peerId {
			// skip the rest
			break
		}
	}

	if peerId != "" && len(peers) == 0 {
		// none found
		return nil, errors.New("Peer " + peerId + " not found")
	}

	list := peerswaprpc.ListPeersResponse{
		Peers: peers,
	}

	return &list, nil
}

func CacheForwards() {
	// not implemented
}
