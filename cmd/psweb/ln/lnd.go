//go:build !cln

package ln

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"peerswap-web/cmd/psweb/bitcoin"
	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/db"

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
	forwardsIn     = make(map[uint64][]*lnrpc.ForwardingEvent)
	forwardsOut    = make(map[uint64][]*lnrpc.ForwardingEvent)
	paymentHtlcs   = make(map[uint64][]*lnrpc.HTLCAttempt)
	rebalanceHtlcs = make(map[uint64][]*lnrpc.HTLCAttempt)
	invoiceHtlcs   = make(map[uint64][]*lnrpc.InvoiceHTLC)

	// inflight HTLCs mapped per Incoming channel
	inflightHTLCs = make(map[uint64][]*InflightHTLC)

	// las index for invoice subscriptions
	lastInvoiceSettleIndex uint64

	// last timestamps for downloads
	lastForwardCreationTs uint64
	lastPaymentCreationTs int64
	lastInvoiceCreationTs int64

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

	downloadComplete bool
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
func SendCoinsWithUtxos(utxos *[]string, addr string, amount int64, feeRate uint64, subtractFeeFromAmount bool, label string) (*SentResult, error) {
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
		Label: label,
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
			//log.Println("ReleaseOutput:", err)
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

func BumpPeginFee(feeRate uint64, label string) (*SentResult, error) {

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
		len(tx.OutputDetails) == 1,
		label)

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
	return getLndVersion() >= 0.18
}

func HasInboundFees() bool {
	return getLndVersion() >= 0.18
}

func getLndVersion() float64 {
	if LndVerson == 0 {
		// get lnd server version
		client, cleanup, err := GetClient()
		if err != nil {
			return 0
		}
		defer cleanup()

		res, err := client.GetInfo(context.Background(), &lnrpc.GetInfoRequest{})
		if err != nil {
			return 0
		}

		version := res.GetVersion()
		parts := strings.Split(version, ".")

		a, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return 0
		}

		LndVerson = a

		b, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return 0
		}

		LndVerson += b / 100

		// save myNodeId
		myNodeId = res.GetIdentityPubkey()
	}

	return LndVerson
}

func GetBlockHeight(client lnrpc.LightningClient) uint32 {
	res, err := client.GetInfo(context.Background(), &lnrpc.GetInfoRequest{})
	if err != nil {
		return 0
	}

	return res.GetBlockHeight()
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

func downloadInvoices(client lnrpc.LightningClient) error {
	// only go back 6 months for itinial download
	start := uint64(time.Now().AddDate(0, -6, 0).Unix())
	offset := uint64(0)
	totalInvoices := uint64(0)

	// incremental download
	if lastInvoiceCreationTs > 0 {
		start = uint64(lastInvoiceCreationTs) + 1
	}

	for {
		res, err := client.ListInvoices(context.Background(), &lnrpc.ListInvoiceRequest{
			CreationDateStart: start,
			Reversed:          false,
			IndexOffset:       offset,
			NumMaxInvoices:    100, // bolt11 fields can be long
		})
		if err != nil {
			if !strings.HasPrefix(fmt.Sprint(err), "rpc error: code = Unknown desc = waiting to start") &&
				!strings.HasPrefix(fmt.Sprint(err), "rpc error: code = Unknown desc = the RPC server is in the process of starting up") {
				log.Println("ListInvoices:", err)
			}
			return err
		}

		for _, invoice := range res.Invoices {
			appendInvoice(invoice)
		}

		n := len(res.Invoices)
		totalInvoices += uint64(n)

		if n > 0 {
			// settle index for subscription
			lastInvoiceSettleIndex = res.Invoices[n-1].SettleIndex
		}
		if n < 100 {
			// all invoices retrieved
			break
		}

		// next pull start from the next index
		offset = res.LastIndexOffset
	}

	if totalInvoices > 0 {
		log.Printf("Cached %d invoices", totalInvoices)
	}

	return nil
}

func downloadForwards(client lnrpc.LightningClient) {
	// only go back 6 months
	start := uint64(time.Now().AddDate(0, -6, 0).Unix())

	// incremental download if substription was interrupted
	if lastForwardCreationTs > 0 {
		// continue from the last timestamp in seconds
		start = lastForwardCreationTs + 1
	}

	// download forwards
	offset := uint32(0)
	totalForwards := uint64(0)
	for {
		res, err := client.ForwardingHistory(context.Background(), &lnrpc.ForwardingHistoryRequest{
			StartTime:       start,
			IndexOffset:     offset,
			PeerAliasLookup: false,
			NumMaxEvents:    50000,
		})
		if err != nil {
			log.Println("ForwardingHistory:", err)
			return
		}

		// sort by in and out channels
		for _, event := range res.ForwardingEvents {
			if event.AmtOutMsat > ignoreForwardsMsat {
				forwardsIn[event.ChanIdIn] = append(forwardsIn[event.ChanIdIn], event)
				forwardsOut[event.ChanIdOut] = append(forwardsOut[event.ChanIdOut], event)
				LastForwardTS[event.ChanIdOut] = int64(event.TimestampNs / 1_000_000_000)
			}
		}

		n := len(res.ForwardingEvents)
		totalForwards += uint64(n)

		if n > 0 {
			// store the last timestamp
			lastForwardCreationTs = res.ForwardingEvents[n-1].TimestampNs / 1_000_000_000
		}
		if n < 50000 {
			// all events retrieved
			break
		}
		// next pull start from the next index
		offset = res.LastOffsetIndex
	}

	if totalForwards > 0 {
		log.Printf("Cached %d forwards", totalForwards)
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
	totalPayments := uint64(0)
	for {
		res, err := client.ListPayments(context.Background(), &lnrpc.ListPaymentsRequest{
			CreationDateStart: start,
			IncludeIncomplete: false,
			Reversed:          false,
			IndexOffset:       offset,
			MaxPayments:       100, // labels can be long
		})
		if err != nil {
			log.Println("ListPayments:", err)
			return
		}

		// will only append settled ones
		for _, payment := range res.Payments {
			appendPayment(payment)
		}

		n := len(res.Payments)
		totalPayments += uint64(n)

		if n > 0 {
			// store the last timestamp
			lastPaymentCreationTs = res.Payments[n-1].CreationTimeNs / 1_000_000_000
		}
		if n < 100 {
			// all events retrieved
			break
		}

		// next pull start from the next index
		offset = res.LastIndexOffset
	}

	if totalPayments > 0 {
		log.Printf("Cached %d payments", totalPayments)
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
					if parts := strings.Split(*invoice.Description, " "); len(parts) > 4 {
						if parts[0] == "peerswap" {
							// find swap id
							if parts[2] == "fee" && len(parts[4]) > 0 {
								// save rebate payment
								saveSwapRabate(parts[4], payment.ValueMsat/1000)
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
				// get destination from the last hop
				chanId = htlc.Route.Hops[len(htlc.Route.Hops)-1].ChanId
				rebalanceHtlcs[chanId] = append(rebalanceHtlcs[chanId], htlc)
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
					forwardingEvent.Fee = forwardingEvent.FeeMsat / 1000
					forwardingEvent.AmtInMsat = info.IncomingAmtMsat
					forwardingEvent.AmtOutMsat = info.OutgoingAmtMsat
					forwardingEvent.AmtIn = info.IncomingAmtMsat / 1000
					forwardingEvent.AmtOut = info.OutgoingAmtMsat / 1000
					forwardingEvent.TimestampNs = htlcEvent.TimestampNs
					forwardingEvent.ChanIdIn = htlcEvent.IncomingChannelId
					forwardingEvent.ChanIdOut = htlcEvent.OutgoingChannelId

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

		case *routerrpc.HtlcEvent_LinkFailEvent:
			// delete from queue
			removeInflightHTLC(htlcEvent.IncomingChannelId, htlcEvent.IncomingHtlcId)

			// check reason
			if event.LinkFailEvent.FailureDetail == routerrpc.FailureDetail_INSUFFICIENT_BALANCE {
				// execute autofee
				client, cleanup, err := GetClient()
				if err != nil {
					return err
				}
				defer cleanup()

				applyAutoFee(client, htlcEvent.OutgoingChannelId, true)
			}

		case *routerrpc.HtlcEvent_SettleEvent:
			// find HTLC in queue
			for _, htlc := range inflightHTLCs[htlcEvent.IncomingChannelId] {
				if htlc.IncomingHtlcId == htlcEvent.IncomingHtlcId {
					// store the last timestamp
					lastForwardCreationTs = htlc.forwardingEvent.TimestampNs / 1_000_000_000
					// delete from queue
					removeInflightHTLC(htlcEvent.IncomingChannelId, htlcEvent.IncomingHtlcId)

					// ignore dust
					if htlc.forwardingEvent.AmtOutMsat > ignoreForwardsMsat {
						// add our stored forwards
						forwardsIn[htlcEvent.IncomingChannelId] = append(forwardsIn[htlcEvent.IncomingChannelId], htlc.forwardingEvent)
						// settled htlcEvent has no Outgoing info, take from queue
						forwardsOut[htlc.OutgoingChannelId] = append(forwardsOut[htlc.OutgoingChannelId], htlc.forwardingEvent)
						// TS for autofee
						LastForwardTS[htlc.OutgoingChannelId] = int64(htlc.forwardingEvent.TimestampNs / 1_000_000_000)

						// execute autofee
						client, cleanup, err := GetClient()
						if err != nil {
							return err
						}
						defer cleanup()

						// calculate with new balance
						applyAutoFee(client, htlc.forwardingEvent.ChanIdOut, false)
						break
					}
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
	if downloadComplete {
		// only run once if successful
		return
	}

	conn, err := lndConnection()
	if err != nil {
		return
	}
	defer conn.Close()

	client := lnrpc.NewLightningClient(conn)
	ctx := context.Background()

	// initial download
	if downloadInvoices(client) != nil {
		return
	}

	downloadComplete = true

	routerClient := routerrpc.NewRouterClient(conn)

	go func() {
		// initial download forwards
		downloadForwards(client)
		// subscribe to Forwards
		for {
			if subscribeForwards(ctx, routerClient) != nil {
				time.Sleep(60 * time.Second)
				// incremental download after error
				downloadForwards(client)
			}
		}
	}()

	go func() {
		// initial download payments
		downloadPayments(client)

		// subscribe to Payments
		for {
			if subscribePayments(ctx, routerClient) != nil {
				time.Sleep(60 * time.Second)
				// incremental download after error
				downloadPayments(client)
			}
		}
	}()

	go func() {
		// subscribe to Messages
		for {
			if subscribeMessages(ctx, client) != nil {
				time.Sleep(60 * time.Second)
			}
		}
	}()

	// subscribe to Invoices
	for {
		if subscribeInvoices(ctx, client) != nil {
			time.Sleep(60 * time.Second)
			// incremental download after error
			downloadInvoices(client)
		}
	}
}

func appendInvoice(invoice *lnrpc.Invoice) {
	if invoice == nil {
		// precaution
		return
	}

	// save for incremental downloads
	lastInvoiceCreationTs = invoice.CreationDate

	// only append settled htlcs
	if invoice.State == lnrpc.Invoice_SETTLED {
		if parts := strings.Split(invoice.Memo, " "); len(parts) > 4 {
			if parts[0] == "peerswap" {
				// find swap id
				if parts[2] == "fee" && len(parts[4]) > 0 {
					// save rebate payment
					saveSwapRabate(parts[4], invoice.AmtPaidMsat/1000)
				}
				// skip peerswap-related
				return
			}
		}
		for _, htlc := range invoice.Htlcs {
			if htlc.State == lnrpc.InvoiceHTLCState_SETTLED {
				invoiceHtlcs[htlc.ChanId] = append(invoiceHtlcs[htlc.ChanId], htlc)
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

func subscribeMessages(ctx context.Context, client lnrpc.LightningClient) error {
	stream, err := client.SubscribeCustomMessages(ctx, &lnrpc.SubscribeCustomMessagesRequest{})
	if err != nil {
		return err
	}

	log.Println("Subscribed to messages")

	for {
		data, err := stream.Recv()
		if err != nil {
			return err
		}
		if data.Type == messageType {
			var msg Message
			err := json.Unmarshal(data.Data, &msg)
			if err != nil {
				log.Println("Received an incorrectly formed message")
				continue
			}
			if msg.Version != MessageVersion {
				log.Println("Received a message with wrong version number")
				continue
			}

			nodeId := hex.EncodeToString(data.Peer)

			// received broadcast of pegin status
			// msg.Asset: "pegin_started" or "pegin_ended"
			if msg.Memo == "broadcast" {
				err = Broadcast(nodeId, &msg)
				if err != nil {
					log.Println(err)
				}
			}

			// messages related to pegin claim join
			if msg.Memo == "process" && config.Config.PeginClaimJoin {
				Process(&msg, nodeId)
			}

			// received request for information
			if msg.Memo == "poll" {
				if MyRole == "initiator" && myPublicKey() != "" && len(ClaimParties) < maxParties && GetBlockHeight(client) < ClaimBlockHeight {
					// repeat pegin start info
					SendCustomMessage(client, nodeId, &Message{
						Version: MessageVersion,
						Memo:    "broadcast",
						Asset:   "pegin_started",
						Amount:  uint64(ClaimBlockHeight),
						Sender:  myPublicKey(),
					})
				}

				if AdvertiseLiquidBalance {
					if SendCustomMessage(client, nodeId, &Message{
						Version: MessageVersion,
						Memo:    "balance",
						Asset:   "lbtc",
						Amount:  LiquidBalance,
					}) == nil {
						// save announcement
						if SentLiquidBalances[nodeId] == nil {
							SentLiquidBalances[nodeId] = new(BalanceInfo)
						}
						SentLiquidBalances[nodeId].Amount = LiquidBalance
						SentLiquidBalances[nodeId].TimeStamp = time.Now().Unix()
					}

					if AdvertiseBitcoinBalance {
						if SendCustomMessage(client, nodeId, &Message{
							Version: MessageVersion,
							Memo:    "balance",
							Asset:   "btc",
							Amount:  BitcoinBalance,
						}) == nil {
							// save announcement
							if SentBitcoinBalances[nodeId] == nil {
								SentBitcoinBalances[nodeId] = new(BalanceInfo)
							}
							SentBitcoinBalances[nodeId].Amount = BitcoinBalance
							SentBitcoinBalances[nodeId].TimeStamp = time.Now().Unix()
						}
					}
				}
			}

			// received information
			if msg.Memo == "balance" {
				ts := time.Now().Unix()
				if msg.Asset == "lbtc" {
					if LiquidBalances[nodeId] == nil {
						LiquidBalances[nodeId] = new(BalanceInfo)
					}
					LiquidBalances[nodeId].Amount = msg.Amount
					LiquidBalances[nodeId].TimeStamp = ts
				}
				if msg.Asset == "btc" {
					if BitcoinBalances[nodeId] == nil {
						BitcoinBalances[nodeId] = new(BalanceInfo)
					}
					BitcoinBalances[nodeId].Amount = msg.Amount
					BitcoinBalances[nodeId].TimeStamp = ts
				}
			}
		}
	}
}

func SendCustomMessage(client lnrpc.LightningClient, peerId string, message *Message) error {
	peerByte, err := hex.DecodeString(peerId)
	if err != nil {
		return err
	}

	data, err := json.Marshal(message)
	if err != nil {
		return err
	}

	log.Println("Message size:", len(data))

	req := &lnrpc.SendCustomMessageRequest{
		Peer: peerByte,
		Type: messageType,
		Data: data,
	}

	_, err = client.SendCustomMessage(context.Background(), req)
	if err != nil {
		return err
	}

	return nil
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
		if e.TimestampNs > timestamp6m && e.AmtOutMsat > ignoreForwardsMsat {
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
		if e.TimestampNs > timestamp6m && e.AmtOutMsat > ignoreForwardsMsat {
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

// get fees for the channel
func GetChannelInfo(client lnrpc.LightningClient, channelId uint64, peerNodeId string) *ChanneInfo {
	info := new(ChanneInfo)

	info.ChannelId = channelId

	r, err := client.GetChanInfo(context.Background(), &lnrpc.ChanInfoRequest{
		ChanId: channelId,
	})
	if err != nil {
		log.Println("GetChannelInfo:", err)
		return info
	}

	policy := r.Node1Policy
	peerPolicy := r.Node2Policy
	if r.Node1Pub == peerNodeId {
		// the first policy is not ours, use the second
		policy = r.Node2Policy
		peerPolicy = r.Node1Policy
	}

	info.FeeRate = policy.GetFeeRateMilliMsat()
	info.FeeBase = policy.GetFeeBaseMsat()
	if HasInboundFees() {
		info.InboundFeeBase = int64(policy.GetInboundFeeBaseMsat())
		info.InboundFeeRate = int64(policy.GetInboundFeeRateMilliMsat())
	}
	info.OurMaxHtlc = policy.GetMaxHtlcMsat() / 1000
	info.OurMinHtlc = msatToSatUp(uint64(policy.GetMinHtlc()))

	info.PeerMaxHtlc = peerPolicy.GetMaxHtlcMsat() / 1000
	info.PeerMinHtlc = msatToSatUp(uint64(peerPolicy.GetMinHtlc()))
	info.PeerFeeRate = peerPolicy.GetFeeRateMilliMsat()
	info.PeerFeeBase = peerPolicy.GetFeeBaseMsat()
	info.PeerInboundFeeBase = int64(peerPolicy.GetInboundFeeBaseMsat())
	info.PeerInboundFeeRate = int64(peerPolicy.GetInboundFeeRateMilliMsat())

	info.Capacity = uint64(r.Capacity)

	return info
}

// flow stats for a channel since timestamp
func GetChannelStats(channelId uint64, timeStamp uint64) *ChannelStats {

	var (
		result            ChannelStats
		routedInMsat      uint64
		routedOutMsat     uint64
		feeMsat           uint64
		assistedMsat      uint64
		paidOutMsat       int64
		invoicedMsat      uint64
		costMsat          int64
		rebalanceMsat     int64
		rebalanceCostMsat int64
	)

	timestampNs := timeStamp * 1_000_000_000

	for _, e := range forwardsOut[channelId] {
		if e.TimestampNs > timestampNs && e.AmtOutMsat > ignoreForwardsMsat {
			routedOutMsat += e.AmtOutMsat
			feeMsat += e.FeeMsat
		}
	}

	for _, e := range forwardsIn[channelId] {
		if e.TimestampNs > timestampNs && e.AmtOutMsat > ignoreForwardsMsat {
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

	for _, e := range rebalanceHtlcs[channelId] {
		if uint64(e.AttemptTimeNs) > timestampNs {
			rebalanceMsat += e.Route.TotalAmtMsat
			rebalanceCostMsat += e.Route.TotalFeesMsat
		}
	}

	result.RoutedOut = routedOutMsat / 1000
	result.RoutedIn = routedInMsat / 1000
	result.FeeSat = feeMsat / 1000
	result.AssistedFeeSat = assistedMsat / 1000
	result.InvoicedIn = invoicedMsat / 1000
	result.PaidOut = uint64(paidOutMsat / 1000)
	result.PaidCost = uint64(costMsat / 1000)
	result.RebalanceIn = uint64(rebalanceMsat / 1000)
	result.RebalanceCost = uint64(rebalanceCostMsat / 1000)

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

// Returns Lightning channels as peerswaprpc.ListPeersResponse, excluding certain nodes
func ListPeers(client lnrpc.LightningClient, peerId string, excludeIds *[]string) (*peerswaprpc.ListPeersResponse, error) {
	ctx := context.Background()

	res, err := client.ListPeers(ctx, &lnrpc.ListPeersRequest{
		LatestError: true,
	})
	if err != nil {
		return nil, err
	}

	res2, err := client.ListChannels(ctx, &lnrpc.ListChannelsRequest{
		ActiveOnly: false,
		PublicOnly: false,
	})
	if err != nil {
		return nil, err
	}

	var peers []*peerswaprpc.PeerSwapPeer

	for _, lndPeer := range res.Peers {
		// skip excluded
		if excludeIds != nil && stringIsInSlice(lndPeer.PubKey, *excludeIds) {
			continue
		}

		// skip if not the single one requested
		if peerId != "" && lndPeer.PubKey != peerId {
			continue
		}

		peer := peerswaprpc.PeerSwapPeer{}
		peer.NodeId = lndPeer.PubKey

		for _, channel := range res2.Channels {
			if channel.RemotePubkey == lndPeer.PubKey {
				peer.Channels = append(peer.Channels, &peerswaprpc.PeerSwapPeerChannel{
					ChannelId:     channel.ChanId,
					LocalBalance:  uint64(channel.LocalBalance + channel.UnsettledBalance),
					RemoteBalance: uint64(channel.RemoteBalance),
					Active:        channel.Active,
				})
			}
		}

		peer.AsSender = &peerswaprpc.SwapStats{}
		peer.AsReceiver = &peerswaprpc.SwapStats{}

		// skip peers with no channels with us
		if len(peer.Channels) > 0 {
			peers = append(peers, &peer)
		}

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

// Estimate sat/vB fee
func EstimateFee() float64 {
	conn, err := lndConnection()
	if err != nil {
		return 0
	}
	cl := walletrpc.NewWalletKitClient(conn)

	ctx := context.Background()
	req := &walletrpc.EstimateFeeRequest{ConfTarget: 2}
	res, err := cl.EstimateFee(ctx, req)

	if err != nil {
		return 0
	}

	return float64(res.SatPerKw / 250)
}

// get fees for all channels by filling the maps [channelId]
func FeeReport(client lnrpc.LightningClient, outboundFeeRates map[uint64]int64, inboundFeeRates map[uint64]int64) error {
	r, err := client.FeeReport(context.Background(), &lnrpc.FeeReportRequest{})
	if err != nil {
		log.Println("FeeReport:", err)
		return err
	}

	for _, ch := range r.ChannelFees {
		outboundFeeRates[ch.ChanId] = ch.FeePerMil
		inboundFeeRates[ch.ChanId] = int64(ch.InboundFeePerMil)
	}

	return nil
}

// set fee rate for a channel, return old rate
func SetFeeRate(peerNodeId string,
	channelId uint64,
	feeRate int64,
	inbound bool,
	isBase bool) (int, error) {

	client, cleanup, err := GetClient()
	if err != nil {
		log.Println("SetFeeRate:", err)
		return 0, err
	}
	defer cleanup()

	// get channel point first
	r, err := client.GetChanInfo(context.Background(), &lnrpc.ChanInfoRequest{
		ChanId: channelId,
	})
	if err != nil {
		log.Println("SetFeeRate:", err)
		return 0, err
	}

	policy := r.Node1Policy
	if r.Node1Pub == peerNodeId {
		// the first policy is not ours, use the second
		policy = r.Node2Policy
	}

	parts := strings.Split(r.ChanPoint, ":")
	outputIndex, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		log.Println("SetFeeRate:", err)
		return 0, err
	}

	var req lnrpc.PolicyUpdateRequest

	req.Scope = &lnrpc.PolicyUpdateRequest_ChanPoint{
		ChanPoint: &lnrpc.ChannelPoint{
			FundingTxid: &lnrpc.ChannelPoint_FundingTxidStr{
				FundingTxidStr: parts[0],
			},
			OutputIndex: uint32(outputIndex),
		},
	}

	// preserve policy values
	req.BaseFeeMsat = policy.FeeBaseMsat
	req.FeeRatePpm = uint32(policy.FeeRateMilliMsat)
	req.TimeLockDelta = policy.TimeLockDelta
	req.MinHtlcMsatSpecified = false
	req.MaxHtlcMsat = policy.MaxHtlcMsat
	req.InboundFee = &lnrpc.InboundFee{
		FeeRatePpm:  policy.InboundFeeRateMilliMsat,
		BaseFeeMsat: policy.InboundFeeBaseMsat,
	}

	oldRate := 0

	// change what's new
	if isBase {
		if inbound {
			oldRate = int(policy.InboundFeeBaseMsat)
			req.InboundFee.BaseFeeMsat = int32(feeRate)
		} else {
			oldRate = int(policy.FeeBaseMsat)
			req.BaseFeeMsat = feeRate
		}
	} else {
		if inbound {
			oldRate = int(policy.InboundFeeRateMilliMsat)
			req.InboundFee.FeeRatePpm = int32(feeRate)
		} else {
			oldRate = int(policy.FeeRateMilliMsat)
			req.FeeRatePpm = uint32(feeRate)
		}
	}

	if oldRate == int(feeRate) {
		return oldRate, errors.New("rate was already set")
	}

	_, err = client.UpdateChannelPolicy(context.Background(), &req)
	if err != nil {
		log.Println("SetFeeRate:", err)
		return oldRate, err
	}

	return oldRate, nil
}

// set min or max HTLC size (Msat!!!) for a channel
func SetHtlcSize(peerNodeId string,
	channelId uint64,
	htlcMsat int64,
	isMax bool) error {

	client, cleanup, err := GetClient()
	if err != nil {
		log.Println("SetHtlcSize:", err)
		return err
	}
	defer cleanup()

	// get channel point first
	r, err := client.GetChanInfo(context.Background(), &lnrpc.ChanInfoRequest{
		ChanId: channelId,
	})
	if err != nil {
		log.Println("SetHtlcSize:", err)
		return err
	}

	policy := r.Node1Policy
	if r.Node1Pub == peerNodeId {
		// the first policy is not ours, use the second
		policy = r.Node2Policy
	}

	parts := strings.Split(r.ChanPoint, ":")
	outputIndex, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		log.Println("SetHtlcSize:", err)
		return err
	}

	var req lnrpc.PolicyUpdateRequest

	req.Scope = &lnrpc.PolicyUpdateRequest_ChanPoint{
		ChanPoint: &lnrpc.ChannelPoint{
			FundingTxid: &lnrpc.ChannelPoint_FundingTxidStr{
				FundingTxidStr: parts[0],
			},
			OutputIndex: uint32(outputIndex),
		},
	}

	// preserve policy values
	req.BaseFeeMsat = policy.FeeBaseMsat
	req.FeeRatePpm = uint32(policy.FeeRateMilliMsat)
	req.TimeLockDelta = policy.TimeLockDelta
	req.MinHtlcMsatSpecified = !isMax
	req.MaxHtlcMsat = policy.MaxHtlcMsat
	req.InboundFee = &lnrpc.InboundFee{
		FeeRatePpm:  policy.InboundFeeRateMilliMsat,
		BaseFeeMsat: policy.InboundFeeBaseMsat,
	}

	// change what's new
	if isMax {
		if uint64(htlcMsat) == policy.MaxHtlcMsat {
			// nothing to do
			return nil
		}
		req.MaxHtlcMsat = uint64(htlcMsat)
	} else {
		if htlcMsat == policy.MinHtlc {
			// nothing to do
			return nil
		}
		req.MinHtlcMsat = uint64(htlcMsat)
	}

	_, err = client.UpdateChannelPolicy(context.Background(), &req)
	if err != nil {
		log.Println("SetHtlcSize:", err)
		return err
	}

	return nil
}

// called after individual HTLC settles or fails
func applyAutoFee(client lnrpc.LightningClient, channelId uint64, htlcFail bool) {

	if !AutoFeeEnabledAll || !AutoFeeEnabled[channelId] {
		return
	}

	params := &AutoFeeDefaults
	if AutoFee[channelId] != nil {
		// channel has custom parameters
		params = AutoFee[channelId]
	}

	ctx := context.Background()
	if myNodeId == "" {
		// populates myNodeId
		if getLndVersion() == 0 {
			return
		}
	}
	r, err := client.GetChanInfo(ctx, &lnrpc.ChanInfoRequest{
		ChanId: channelId,
	})
	if err != nil {
		return
	}

	policy := r.Node1Policy
	peerId := r.Node2Pub
	if r.Node1Pub != myNodeId {
		// the first policy is not ours, use the second
		policy = r.Node2Policy
		peerId = r.Node1Pub
	}

	oldFee := int(policy.FeeRateMilliMsat)
	newFee := oldFee

	// get balances
	bytePeer, err := hex.DecodeString(peerId)
	if err != nil {
		return
	}

	res, err := client.ListChannels(ctx, &lnrpc.ListChannelsRequest{
		Peer: bytePeer,
	})
	if err != nil {
		return
	}

	localBalance := int64(0)
	unsettledBalance := int64(0)
	for _, ch := range res.Channels {
		if ch.ChanId == channelId {
			localBalance = ch.LocalBalance + ch.UnsettledBalance
			unsettledBalance = ch.UnsettledBalance
			break
		}
	}

	liqPct := int(localBalance * 100 / r.Capacity)

	if htlcFail {
		if liqPct < params.LowLiqPct {
			// increase fee to help prevent further failed HTLCs
			newFee += params.FailedBumpPPM

			// bump LowLiqRate
			if AutoFee[channelId] == nil {
				// add custom parameters
				AutoFee[channelId] = new(AutoFeeParams)
				// clone default values
				*AutoFee[channelId] = AutoFeeDefaults
			}

			AutoFee[channelId].LowLiqRate = newFee
			// persist to db
			db.Save("AutoFees", "AutoFee", AutoFee)
		} else if liqPct > params.LowLiqPct {
			// move threshold
			moveLowLiqThreshold(channelId, params.FailedMoveThreshold)
			return
		}
	} else {
		newFee = calculateAutoFee(channelId, params, liqPct, oldFee)
	}

	// set the new rate
	if newFee != oldFee {
		if unsettledBalance > 0 && newFee < oldFee {
			// do not lower fees for temporary balance spikes due to pending HTLCs
			return
		}

		// check if the fee was already set
		if lastFeeIsTheSame(channelId, newFee, false) {
			return
		}

		if old, err := SetFeeRate(peerId, channelId, int64(newFee), false, false); err == nil {
			if !lastFeeIsTheSame(channelId, newFee, false) {
				// log the last change
				LogFee(channelId, old, newFee, false, false)
			}
		}
	}
}

// review all fees on timer
func ApplyAutoFees() {

	if !AutoFeeEnabledAll {
		return
	}

	client, cleanup, err := GetClient()
	if err != nil {
		return
	}
	defer cleanup()

	ctx := context.Background()
	if myNodeId == "" {
		// populates myNodeId
		if getLndVersion() == 0 {
			return
		}
	}

	res, err := client.ListChannels(ctx, &lnrpc.ListChannelsRequest{
		ActiveOnly: true,
	})
	if err != nil {
		return
	}

	for _, ch := range res.Channels {
		if !AutoFeeEnabled[ch.ChanId] {
			continue
		}

		params := &AutoFeeDefaults
		if AutoFee[ch.ChanId] != nil {
			// channel has custom parameters
			params = AutoFee[ch.ChanId]
		}

		r, err := client.GetChanInfo(ctx, &lnrpc.ChanInfoRequest{
			ChanId: ch.ChanId,
		})
		if err != nil {
			return
		}

		policy := r.Node1Policy
		peerId := r.Node2Pub
		if r.Node1Pub != myNodeId {
			// the first policy is not ours, use the second
			policy = r.Node2Policy
			peerId = r.Node1Pub
		}

		oldFee := int(policy.FeeRateMilliMsat)
		liqPct := int((ch.LocalBalance + ch.UnsettledBalance) * 100 / r.Capacity)

		newFee := calculateAutoFee(ch.ChanId, params, liqPct, oldFee)

		// set the new rate
		if newFee != oldFee {
			if ch.UnsettledBalance > 0 && newFee < oldFee {
				// do not lower fees during pending HTLCs
				continue
			}

			// check if the fee was already set
			if lastFeeIsTheSame(ch.ChanId, newFee, false) {
				continue
			}

			_, err := SetFeeRate(peerId, ch.ChanId, int64(newFee), false, false)
			if err == nil && !lastFeeIsTheSame(ch.ChanId, newFee, false) {
				// log the last change
				LogFee(ch.ChanId, oldFee, newFee, false, false)
			}
		}

		// do not change inbound fee during pending HTLCs
		if HasInboundFees() && ch.UnsettledBalance == 0 {
			toSet := false
			discountRate := int64(0)

			if liqPct < params.LowLiqPct && policy.InboundFeeRateMilliMsat > int32(params.LowLiqDiscount) {
				// set inbound fee discount
				discountRate = int64(params.LowLiqDiscount)
				toSet = true
			} else if liqPct > params.LowLiqPct && policy.InboundFeeRateMilliMsat < 0 {
				// remove discount unless it was set manually or CoolOffHours did not pass
				lastFee := LastAutoFeeLog(ch.ChanId, true)
				if lastFee != nil {
					if !lastFee.IsManual && lastFee.TimeStamp < time.Now().Add(-time.Duration(params.CoolOffHours)*time.Hour).Unix() {
						toSet = true
					}
				}
			}

			if toSet && !lastFeeIsTheSame(ch.ChanId, int(discountRate), true) {
				oldRate, err := SetFeeRate(peerId, ch.ChanId, discountRate, true, false)
				if err == nil && !lastFeeIsTheSame(ch.ChanId, int(discountRate), true) {
					// log the last change
					LogFee(ch.ChanId, oldRate, int(discountRate), true, false)
				}
			}
		}
	}
}

func PlotPPM(channelId uint64) *[]DataPoint {
	var plot []DataPoint

	for _, e := range forwardsOut[channelId] {
		// ignore small forwards
		if e.AmtOutMsat > ignoreForwardsMsat {
			plot = append(plot, DataPoint{
				TS:     e.TimestampNs / 1_000_000_000,
				Amount: e.AmtOut,
				Fee:    float64(e.FeeMsat) / 1000,
				PPM:    e.FeeMsat * 1_000_000 / e.AmtOutMsat,
			})
		}
	}

	return &plot
}

// channelId == 0 means all channels
func ForwardsLog(channelId uint64, fromTS int64) *[]DataPoint {
	var log []DataPoint
	fromTS_Ns := uint64(fromTS * 1_000_000_000)

	for chId := range forwardsOut {
		if channelId > 0 && channelId != chId {
			continue
		}
		for _, e := range forwardsOut[chId] {
			// ignore small forwards
			if e.AmtOutMsat > ignoreForwardsMsat && e.TimestampNs >= fromTS_Ns {
				log = append(log, DataPoint{
					TS:        e.TimestampNs / 1_000_000_000,
					Amount:    e.AmtOut,
					Fee:       float64(e.FeeMsat) / 1000,
					PPM:       e.FeeMsat * 1_000_000 / e.AmtOutMsat,
					ChanIdIn:  e.ChanIdIn,
					ChanIdOut: e.ChanIdOut,
				})
			}
		}
	}

	if channelId > 0 {
		for _, e := range forwardsIn[channelId] {
			// ignore small forwards
			if e.AmtOutMsat > ignoreForwardsMsat && e.TimestampNs >= fromTS_Ns {
				log = append(log, DataPoint{
					TS:        e.TimestampNs / 1_000_000_000,
					Amount:    e.AmtOut,
					Fee:       float64(e.FeeMsat) / 1000,
					PPM:       e.FeeMsat * 1_000_000 / e.AmtOutMsat,
					ChanIdIn:  e.ChanIdIn,
					ChanIdOut: e.ChanIdOut,
				})
			}
		}
	}

	// sort by TimeStamp descending
	sort.Slice(log, func(i, j int) bool {
		return log[i].TS > log[j].TS
	})

	return &log
}
