//go:build !cln

package ln

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"peerswap-web/cmd/psweb/bitcoin"
	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/db"
	"peerswap-web/cmd/psweb/safemap"

	"github.com/elementsproject/peerswap/peerswaprpc"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
)

const (
	IMPLEMENTATION = "LND"
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
	forwardsIn        = safemap.New[uint64, []*lnrpc.ForwardingEvent]()
	forwardsOut       = safemap.New[uint64, []*lnrpc.ForwardingEvent]()
	paymentHtlcs      = safemap.New[uint64, []*lnrpc.HTLCAttempt]()
	rebalanceInHtlcs  = safemap.New[uint64, []*lnrpc.HTLCAttempt]()
	rebalanceOutHtlcs = safemap.New[uint64, []*lnrpc.HTLCAttempt]()
	invoiceHtlcs      = safemap.New[uint64, []*lnrpc.InvoiceHTLC]()

	// inflight HTLCs mapped per Incoming channel
	inflightHTLCs = safemap.New[uint64, []*InflightHTLC]()

	// cache peer addresses for reconnects
	peerAddresses = safemap.New[string, []*lnrpc.NodeAddress]()

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
		grpc.WithPerRPCCredentials(macCred),
	}

	conn, err := grpc.NewClient(host, opts...)
	if err != nil {
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
func SendCoinsWithUtxos(utxos *[]string, addr string, amount int64, feeRate float64, subtractFeeFromAmount bool, label string) (*SentResult, error) {
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
		// new template since LND 0.18+
		// change lockID to custom and construct manual psbt
		lockId = myLockId
		psbtBytes, err = fundPsbtSpendAll(cl, utxos, addr, uint64(feeRate))
		if err != nil {
			return nil, err
		}
	} else {
		psbtBytes, err = fundPsbt(cl, utxos, outputs, uint64(feeRate))
		if err != nil {
			log.Println("FundPsbt:", err)
			return nil, err
		}

		if subtractFeeFromAmount {
			// trick for LND before 0.18
			// replace output with correct address and amount
			fee, err := bitcoin.GetFeeFromPsbt(base64.StdEncoding.EncodeToString(psbtBytes))
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
				psbtBytes, err = fundPsbt(cl, utxos, outputs, uint64(feeRate))

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

	pass := 0

finalize:
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

	decoded, err := bitcoin.DecodeRawTransaction(hex.EncodeToString(rawTx))
	if err != nil {
		//log.Println("Funded PSBT:", hex.EncodeToString(psbtBytes))
		return nil, err
	}

	feePaid, err := bitcoin.GetFeeFromPsbt(base64.StdEncoding.EncodeToString(psbtBytes))
	if err != nil {
		return nil, err
	}

	requiredFee := int64(feeRate * float64(decoded.VSize))

	if requiredFee != toSats(feePaid) {
		if pass < 3 || requiredFee > toSats(feePaid) {
			log.Println("Trying to fix fee paid", toSats(feePaid), "vs required", requiredFee)

			releaseOutputs(cl, utxos, &lockId)

			// Parse the PSBT
			p, err := psbt.NewFromRawBytes(bytes.NewReader(psbtBytes), false)
			if err != nil {
				log.Println("NewFromRawBytes:", err)
				return nil, err
			}

			// Output index to amend (destination or change)
			outputIndex := len(p.UnsignedTx.TxOut) - 1

			// Replace the value of the output
			p.UnsignedTx.TxOut[outputIndex].Value -= requiredFee - toSats(feePaid)

			// Serialize the PSBT back to raw bytes
			var buf bytes.Buffer
			err = p.Serialize(&buf)
			if err != nil {
				log.Println("Serialize:", err)
				return nil, err
			}

			psbtBytes = buf.Bytes()

			// Print the updated PSBT in hex
			// log.Println(base64.StdEncoding.EncodeToString(psbtBytes))

			// avoid permanent loop
			pass++

			goto finalize
		}

		// did not fix in 5 passes, give up
		log.Println("Unable to fix fee paid", toSats(feePaid), "vs required", requiredFee)
	}

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
		// log.Println("RawHex:", hex.EncodeToString(rawTx))
		releaseOutputs(cl, utxos, &lockId)
		return nil, err
	}

	// confirm the final amount sent
	if subtractFeeFromAmount {
		finalAmount = msgTx.TxOut[0].Value
	}

	result := SentResult{
		RawHex:     hex.EncodeToString(rawTx),
		TxId:       msgTx.TxHash().String(),
		AmountSat:  finalAmount,
		ExactSatVb: math.Ceil(float64(toSats(feePaid)*1000)/float64(decoded.VSize)) / 1000,
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

	parsed, err := btcutil.DecodeAddress(address, getHarnessNetParams())
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

func BumpPeginFee(feeRate float64, label string) (*SentResult, error) {

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
		err = doCPFP(cl, tx.GetOutputDetails(), uint64(feeRate))
		if err != nil {
			return nil, err
		} else {
			return &SentResult{
				TxId:       config.Config.PeginTxId,
				RawHex:     "",
				AmountSat:  config.Config.PeginAmount,
				ExactSatVb: float64(feeRate),
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

	var ress *SentResult
	var errr error

	// extra bump may be necessary if the tx does not pay enough fee
	for extraBump := float64(0); extraBump <= 1; extraBump += 0.01 {
		// sometimes remove transaction is not enough
		releaseOutputs(cl, &utxos, &internalLockId)
		releaseOutputs(cl, &utxos, &myLockId)

		ress, errr = SendCoinsWithUtxos(
			&utxos,
			config.Config.PeginAddress,
			config.Config.PeginAmount,
			feeRate+extraBump,
			len(tx.OutputDetails) == 1,
			label)
		if errr != nil && errr.Error() == "rpc error: code = Unknown desc = insufficient fee" {
			continue
		} else {
			break
		}
	}

	return ress, errr
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
	}

	return LndVerson
}

func GetBlockHeight() uint32 {
	client, cleanup, err := GetClient()
	if err != nil {
		return 0
	}
	defer cleanup()

	res, err := client.GetInfo(context.Background(), &lnrpc.GetInfoRequest{})
	if err != nil {
		return 0
	}

	return res.GetBlockHeight()
}

// return false on error
func downloadInvoices(client lnrpc.LightningClient) bool {
	// only go back 6 months for itinial download
	startTs := uint64(time.Now().AddDate(0, -6, 0).Unix())
	offset := uint64(0)
	totalInvoices := uint64(0)

	// incremental download
	if lastInvoiceCreationTs > 0 {
		startTs = uint64(lastInvoiceCreationTs) + 1
	}

	// benchmark time
	start := time.Now()

	for {
		res, err := client.ListInvoices(context.Background(), &lnrpc.ListInvoiceRequest{
			CreationDateStart: startTs,
			Reversed:          false,
			IndexOffset:       offset,
			NumMaxInvoices:    100, // bolt11 fields can be long
		})
		if err != nil {
			if !strings.HasPrefix(fmt.Sprint(err), "rpc error: code = Unknown desc = waiting to start") &&
				!strings.HasPrefix(fmt.Sprint(err), "rpc error: code = Unknown desc = the RPC server is in the process of starting up") {
				log.Println("ListInvoices:", err)
			}
			return false
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
		duration := time.Since(start)
		log.Printf("Cached %d invoices in %.2f seconds", totalInvoices, duration.Seconds())
	}

	return true
}

// return false on error
func downloadForwards(client lnrpc.LightningClient) bool {
	// only go back 6 months
	startTs := uint64(time.Now().AddDate(0, -6, 0).Unix())

	// incremental download if substription was interrupted
	if lastForwardCreationTs > 0 {
		// continue from the last timestamp in seconds
		startTs = lastForwardCreationTs + 1
	}

	// benchmark time
	start := time.Now()

	// download forwards
	offset := uint32(0)
	totalForwards := uint64(0)
	for {
		res, err := client.ForwardingHistory(context.Background(), &lnrpc.ForwardingHistoryRequest{
			StartTime:       startTs,
			IndexOffset:     offset,
			PeerAliasLookup: false,
			NumMaxEvents:    50000,
		})
		if err != nil {
			if !strings.HasPrefix(fmt.Sprint(err), "rpc error: code = Unknown desc = waiting to start") &&
				!strings.HasPrefix(fmt.Sprint(err), "rpc error: code = Unknown desc = the RPC server is in the process of starting up") {
				log.Println("ForwardingHistory:", err)
			}
			return false // lnd not ready
		}

		// sort by in and out channels
		for _, event := range res.ForwardingEvents {
			if event.AmtOutMsat >= IGNORE_FORWARDS_MSAT {
				fi, ok := forwardsIn.Read(event.ChanIdIn)
				if !ok {
					fi = []*lnrpc.ForwardingEvent{} // Initialize an empty slice if the key does not exist
				}
				fi = append(fi, event)               // Append the new forwarding record
				forwardsIn.Write(event.ChanIdIn, fi) // Write the updated slice

				fo, ok := forwardsOut.Read(event.ChanIdOut)
				if !ok {
					fo = []*lnrpc.ForwardingEvent{} // Initialize an empty slice if the key does not exist
				}
				fo = append(fo, event)                 // Append the new forwarding record
				forwardsOut.Write(event.ChanIdOut, fo) // Write the updated slice

				LastForwardTS.Write(event.ChanIdOut, int64(event.TimestampNs/1_000_000_000))
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
		duration := time.Since(start)
		log.Printf("Cached %d forwards in %.2f seconds", totalForwards, duration.Seconds())
	}

	return true
}

// return false on error
func downloadPayments(client lnrpc.LightningClient) bool {
	// only go back 6 months
	startTs := uint64(time.Now().AddDate(0, -6, 0).Unix())

	// incremental download if substription was interrupted
	if lastPaymentCreationTs > 0 {
		// continue from the last timestamp in seconds
		startTs = uint64(lastPaymentCreationTs + 1)
	}

	// benchmark time
	start := time.Now()

	offset := uint64(0)
	totalPayments := uint64(0)
	for {
		res, err := client.ListPayments(context.Background(), &lnrpc.ListPaymentsRequest{
			CreationDateStart: startTs,
			IncludeIncomplete: false,
			Reversed:          false,
			IndexOffset:       offset,
			MaxPayments:       100, // labels can be long
		})
		if err != nil {
			if !strings.HasPrefix(fmt.Sprint(err), "rpc error: code = Unknown desc = waiting to start") &&
				!strings.HasPrefix(fmt.Sprint(err), "rpc error: code = Unknown desc = the RPC server is in the process of starting up") {
				log.Println("ListPayments:", err)
			}
			return false
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
		duration := time.Since(start)
		log.Printf("Cached %d payments in %.2f seconds", totalPayments, duration.Seconds())
	}
	return true
}

func appendPayment(payment *lnrpc.Payment) {
	if payment == nil {
		return
	}

	if payment.Status == lnrpc.Payment_SUCCEEDED {
		if DecodeAndProcessInvoice(payment.PaymentRequest, payment.ValueMsat) {
			// related to peerswap
			return
		}
		for _, htlc := range payment.Htlcs {
			if htlc.Status == lnrpc.HTLCAttempt_SUCCEEDED {
				// get channel from the first hop
				chanId := htlc.Route.Hops[0].ChanId
				// get destination from the last hop
				lastHop := htlc.Route.Hops[len(htlc.Route.Hops)-1]
				if lastHop.PubKey == MyNodeId {
					// this is a circular rebalancing
					htlcs, ok := rebalanceOutHtlcs.Read(chanId)
					if !ok {
						htlcs = []*lnrpc.HTLCAttempt{} // Initialize an empty slice if the key does not exist
					}
					htlcs = append(htlcs, htlc)            // Append the new forwarding record
					rebalanceOutHtlcs.Write(chanId, htlcs) // Write the updated slice

					htlcs, ok = rebalanceInHtlcs.Read(lastHop.ChanId)
					if !ok {
						htlcs = []*lnrpc.HTLCAttempt{} // Initialize an empty slice if the key does not exist
					}
					htlcs = append(htlcs, htlc)                   // Append the new forwarding record
					rebalanceInHtlcs.Write(lastHop.ChanId, htlcs) // Write the updated slice
				} else {
					htlcs, ok := paymentHtlcs.Read(chanId)
					if !ok {
						htlcs = []*lnrpc.HTLCAttempt{} // Initialize an empty slice if the key does not exist
					}
					htlcs = append(htlcs, htlc)       // Append the new forwarding record
					paymentHtlcs.Write(chanId, htlcs) // Write the updated slice
				}
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

					htlcs, ok := inflightHTLCs.Read(htlcEvent.IncomingChannelId)
					if !ok {
						htlcs = []*InflightHTLC{} // Initialize an empty slice if the key does not exist
					}
					htlcs = append(htlcs, htlc)                             // Append the new forwarding record
					inflightHTLCs.Write(htlcEvent.IncomingChannelId, htlcs) // Write the updated slice
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
			htlcs, ok := inflightHTLCs.Read(htlcEvent.IncomingChannelId)
			if ok {
				for _, htlc := range htlcs {
					if htlc.IncomingHtlcId == htlcEvent.IncomingHtlcId {
						// store the last timestamp
						lastForwardCreationTs = htlc.forwardingEvent.TimestampNs / 1_000_000_000
						// delete from queue
						removeInflightHTLC(htlcEvent.IncomingChannelId, htlcEvent.IncomingHtlcId)

						// ignore dust
						if htlc.forwardingEvent.AmtOutMsat >= IGNORE_FORWARDS_MSAT {
							// add our stored forwards
							fi, ok := forwardsIn.Read(htlcEvent.IncomingChannelId)
							if !ok {
								fi = []*lnrpc.ForwardingEvent{} // Initialize an empty slice if the key does not exist
							}
							fi = append(fi, htlc.forwardingEvent)             // Append the new forwarding record
							forwardsIn.Write(htlcEvent.IncomingChannelId, fi) // Write the updated slice

							// settled htlcEvent has no Outgoing info, take from queue
							fo, ok := forwardsOut.Read(htlc.OutgoingChannelId)
							if !ok {
								fo = []*lnrpc.ForwardingEvent{} // Initialize an empty slice if the key does not exist
							}
							fo = append(fo, htlc.forwardingEvent)         // Append the new forwarding record
							forwardsOut.Write(htlc.OutgoingChannelId, fo) // Write the updated slice

							// TS for autofee
							LastForwardTS.Write(htlc.OutgoingChannelId, int64(htlc.forwardingEvent.TimestampNs/1_000_000_000))

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
}

// Function to remove an InflightHTLC object from a slice in the map by IncomingChannelId
func removeInflightHTLC(incomingChannelId, incomingHtlcId uint64) {
	// Retrieve the slice from the map
	htlcSlice, exists := inflightHTLCs.Read(incomingChannelId)
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
		inflightHTLCs.Write(incomingChannelId, append(htlcSlice[:index], htlcSlice[index+1:]...))
	}

	// If the slice becomes empty after removal, delete the map entry
	if htlcs, ok := inflightHTLCs.Read(incomingChannelId); ok && len(htlcs) == 0 {
		inflightHTLCs.Delete(incomingChannelId)
	}
}

// cache all and subscribe
// return false if lightning did not start yet
func DownloadAll() bool {
	if downloadComplete {
		// only run once if successful
		return true
	}

	conn, err := lndConnection()
	if err != nil {
		return false
	}
	//defer conn.Close()

	client := lnrpc.NewLightningClient(conn)
	ctx := context.Background()

	if MyNodeId == "" {
		res, err := client.GetInfo(ctx, &lnrpc.GetInfoRequest{})
		if err != nil {
			// lnd not ready
			return false
		}
		if !res.SyncedToChain || !res.SyncedToGraph {
			// lnd not ready
			return false
		}
		MyNodeAlias = res.GetAlias()
		MyNodeId = res.GetIdentityPubkey()
	}

	// initial download
	if !downloadInvoices(client) {
		return false
	}

	downloadComplete = true

	// subscribe to Invoices
	go func() {
		for {
			if subscribeInvoices(ctx, client) != nil {
				for {
					time.Sleep(60 * time.Second)
					// incremental download after error
					if downloadInvoices(client) {
						break // lnd is alive again
					}
				}
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

	// subscribe to chain blocks
	go func() {
		for {
			if subscribeBlocks(conn) != nil {
				time.Sleep(60 * time.Second)
			}
		}
	}()

	routerClient := routerrpc.NewRouterClient(conn)

	// initial download forwards
	downloadForwards(client)

	go func() {
		// subscribe to Forwards
		for {
			if subscribeForwards(ctx, routerClient) != nil {
				// incremental download after error
				for {
					time.Sleep(60 * time.Second)
					if downloadForwards(client) {
						break // lnd is alive again
					}
				}
			}
		}
	}()

	// initial download payments
	downloadPayments(client)

	go func() {
		// subscribe to Payments
		for {
			if subscribePayments(ctx, routerClient) != nil {
				for {
					time.Sleep(60 * time.Second)
					// incremental download after error
					if downloadPayments(client) {
						break
					}
				}
			}
		}
	}()

	return true
}

func subscribeBlocks(conn *grpc.ClientConn) error {

	client := chainrpc.NewChainNotifierClient(conn)
	ctx := context.Background()
	stream, err := client.RegisterBlockEpochNtfn(ctx, &chainrpc.BlockEpoch{})
	if err != nil {
		return err
	}

	log.Println("Subscribed to blocks")

	for {
		blockEpoch, err := stream.Recv()
		if err != nil {
			return err
		}

		OnBlock(blockEpoch.Height)
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
		if processInvoice(invoice.Memo, invoice.AmtPaidMsat) {
			// skip peerswap-related
			return
		}
		for _, htlc := range invoice.Htlcs {
			if htlc.State == lnrpc.InvoiceHTLCState_SETTLED {
				inv, ok := invoiceHtlcs.Read(htlc.ChanId)
				if !ok {
					inv = []*lnrpc.InvoiceHTLC{} // Initialize an empty slice if the key does not exist
				}
				inv = append(inv, htlc)              // Append the new forwarding record
				invoiceHtlcs.Write(htlc.ChanId, inv) // Write the updated slice
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

	log.Println("Subscribed to custom messages")

	for {
		data, err := stream.Recv()
		if err != nil {
			return err
		}

		if data.Type == MESSAGE_TYPE {
			nodeId := hex.EncodeToString(data.Peer)

			OnMyCustomMessage(nodeId, data.Data)

			if _, ok := peerAddresses.Read(nodeId); !ok {
				// cache peer addresses for reconnects
				info, err := client.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{PubKey: nodeId, IncludeChannels: false})
				if err == nil {
					peerAddresses.Write(nodeId, info.Node.Addresses)
				}
			}
		}
	}
}

func SendCustomMessage(peerId string, message *Message) error {
	client, cleanup, err := GetClient()
	if err != nil {
		return err
	}
	defer cleanup()

	peerByte, err := hex.DecodeString(peerId)
	if err != nil {
		return err
	}

	// Serialize the message using gob
	var buffer bytes.Buffer
	encoder := gob.NewEncoder(&buffer)
	if err := encoder.Encode(message); err != nil {
		return err
	}

	req := &lnrpc.SendCustomMessageRequest{
		Peer: peerByte,
		Type: MESSAGE_TYPE,
		Data: buffer.Bytes(),
	}

	_, err = client.SendCustomMessage(context.Background(), req)
	if err != nil {
		if err.Error() == "rpc error: code = NotFound desc = peer is not connected" {
			// reconnect
			if reconnectPeer(client, peerId) {
				// try again
				_, err = client.SendCustomMessage(context.Background(), req)
				if err == nil {
					goto success
				}
			}
		}
		return err
	}

success:
	// log.Printf("Sent %d bytes %s to %s", len(req.Data), message.Memo, GetAlias(peerId))

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

	fo, ok := forwardsOut.Read(channelId)
	if ok {
		for _, e := range fo {
			if e.TimestampNs > timestamp6m && e.AmtOutMsat >= IGNORE_FORWARDS_MSAT {
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
	}

	fi, ok := forwardsIn.Read(channelId)
	if ok {
		for _, e := range fi {
			if e.TimestampNs > timestamp6m && e.AmtOutMsat >= IGNORE_FORWARDS_MSAT {
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
		rebalanceInMsat   int64
		rebalanceOutMsat  int64
		rebalanceCostMsat int64
	)

	timestampNs := timeStamp * 1_000_000_000

	fo, ok := forwardsOut.Read(channelId)
	if ok {
		for _, e := range fo {
			if e.TimestampNs > timestampNs && e.AmtOutMsat >= IGNORE_FORWARDS_MSAT {
				routedOutMsat += e.AmtOutMsat
				feeMsat += e.FeeMsat
			}
		}
	}

	fi, ok := forwardsIn.Read(channelId)
	if ok {
		for _, e := range fi {
			if e.TimestampNs > timestampNs && e.AmtOutMsat >= IGNORE_FORWARDS_MSAT {
				routedInMsat += e.AmtInMsat
				assistedMsat += e.FeeMsat
			}
		}
	}

	inv, ok := invoiceHtlcs.Read(channelId)
	if ok {
		for i := 0; i < len(inv); i++ {
			e := inv[i]
			if uint64(e.AcceptTime) > timeStamp {
				// check if it is related to a circular rebalancing
				found := false
				htcls, ok := rebalanceInHtlcs.Read(channelId)
				if ok {
					for _, r := range htcls {
						if e.AmtMsat == uint64(r.Route.TotalAmtMsat-r.Route.TotalFeesMsat) {
							found = true
							break
						}
					}
				}
				if found {
					// remove invoice to avoid double counting
					inv = append(inv[:i], inv[i+1:]...)
					invoiceHtlcs.Write(channelId, inv)
					i--
				} else {
					invoicedMsat += e.AmtMsat
				}
			}
		}
	}

	htlcs, ok := paymentHtlcs.Read(channelId)
	if ok {
		for _, e := range htlcs {
			if uint64(e.AttemptTimeNs) > timestampNs {
				paidOutMsat += e.Route.TotalAmtMsat
				costMsat += e.Route.TotalFeesMsat
			}
		}
	}

	htcls, ok := rebalanceInHtlcs.Read(channelId)
	if ok {
		for _, e := range htcls {
			if uint64(e.AttemptTimeNs) > timestampNs {
				rebalanceInMsat += e.Route.TotalAmtMsat - e.Route.TotalFeesMsat
				rebalanceCostMsat += e.Route.TotalFeesMsat
			}
		}
	}

	htcls, ok = rebalanceOutHtlcs.Read(channelId)
	if ok {
		for _, e := range htcls {
			if uint64(e.AttemptTimeNs) > timestampNs {
				rebalanceOutMsat += e.Route.TotalAmtMsat
			}
		}
	}

	result.RoutedOut = routedOutMsat / 1000
	result.RoutedIn = routedInMsat / 1000
	result.FeeSat = feeMsat / 1000
	result.AssistedFeeSat = assistedMsat / 1000
	result.InvoicedIn = invoicedMsat / 1000
	result.PaidOut = uint64(paidOutMsat / 1000)
	result.PaidCost = uint64(costMsat / 1000)
	result.RebalanceIn = uint64(rebalanceInMsat / 1000)
	result.RebalanceOut = uint64(rebalanceOutMsat / 1000)
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
		Type: lnrpc.AddressType_TAPROOT_PUBKEY,
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
	res, err := client.ListChannels(ctx, &lnrpc.ListChannelsRequest{
		ActiveOnly: false,
		PublicOnly: false,
	})
	if err != nil {
		return nil, err
	}

	var peers []*peerswaprpc.PeerSwapPeer

	for _, channel := range res.Channels {
		// skip excluded
		if excludeIds != nil && stringIsInSlice(channel.RemotePubkey, *excludeIds) {
			continue
		}

		// skip if not the single one requested
		if peerId != "" && channel.RemotePubkey != peerId {
			continue
		}

		var peer *peerswaprpc.PeerSwapPeer
		found := false

		// find if peer has been added already
		for i, p := range peers {
			if p.NodeId == channel.RemotePubkey {
				// point to existing
				peer = peers[i]
				found = true
				break
			}
		}

		if !found {
			// make new
			peer = new(peerswaprpc.PeerSwapPeer)
			peer.NodeId = channel.RemotePubkey
			peer.AsSender = &peerswaprpc.SwapStats{}
			peer.AsReceiver = &peerswaprpc.SwapStats{}
			// append to list
			peers = append(peers, peer)
		}
		// append channel
		peer.Channels = append(peer.Channels, &peerswaprpc.PeerSwapPeerChannel{
			ChannelId:     channel.ChanId,
			LocalBalance:  uint64(channel.LocalBalance + channel.UnsettledBalance),
			RemoteBalance: uint64(channel.RemoteBalance),
			Active:        channel.Active,
		})
	}

	if peerId != "" && len(peers) != 1 {
		// none found
		return nil, errors.New("Peer " + peerId + " not found")
	}

	return &peerswaprpc.ListPeersResponse{
		Peers: peers,
	}, nil
}

// Estimate sat/vB fee
func EstimateFee() float64 {
	conn, err := lndConnection()
	if err != nil {
		return 0
	}
	cl := walletrpc.NewWalletKitClient(conn)

	// use 6 blocks estimate
	req := &walletrpc.EstimateFeeRequest{ConfTarget: 6}
	res, err := cl.EstimateFee(context.Background(), req)

	if err != nil {
		return 0
	}

	return math.Round(float64(res.SatPerKw / 250))
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
	r, err := client.GetChanInfo(ctx, &lnrpc.ChanInfoRequest{
		ChanId: channelId,
	})
	if err != nil {
		return
	}

	policy := r.Node1Policy
	peerId := r.Node2Pub
	if r.Node1Pub != MyNodeId {
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
		if r.Node1Pub != MyNodeId {
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

	fo, ok := forwardsOut.Read(channelId)
	if ok {
		for _, e := range fo {
			// ignore small forwards
			if e.AmtOutMsat >= IGNORE_FORWARDS_MSAT {
				plot = append(plot, DataPoint{
					TS:     e.TimestampNs / 1_000_000_000,
					Amount: e.AmtOut,
					Fee:    float64(e.FeeMsat) / 1000,
					PPM:    e.FeeMsat * 1_000_000 / e.AmtOutMsat,
				})
			}
		}
	}

	return &plot
}

// channelId == 0 means all channels
func ForwardsLog(channelId uint64, fromTS int64) *[]DataPoint {
	var log []DataPoint
	fromTS_Ns := uint64(fromTS * 1_000_000_000)

	// Process forwards from forwardsOut
	forwardsOut.Iterate(func(chId uint64, fo []*lnrpc.ForwardingEvent) {
		if channelId > 0 && channelId != chId {
			return
		}
		for _, e := range fo {
			// ignore small forwards
			if e.AmtOutMsat >= IGNORE_FORWARDS_MSAT && e.TimestampNs >= fromTS_Ns {
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
	})

	if channelId > 0 {
		fi, ok := forwardsIn.Read(channelId)
		if ok {
			for _, e := range fi {
				// ignore small forwards
				if e.AmtOutMsat >= IGNORE_FORWARDS_MSAT && e.TimestampNs >= fromTS_Ns {
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
	}

	// sort by TimeStamp descending
	sort.Slice(log, func(i, j int) bool {
		return log[i].TS > log[j].TS
	})

	return &log
}

func reconnectPeer(client lnrpc.LightningClient, nodeId string) bool {
	ctx := context.Background()
	addresses, ok := peerAddresses.Read(nodeId)

	if !ok {
		info, err := client.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{PubKey: nodeId, IncludeChannels: false})
		if err == nil {
			addresses = info.Node.Addresses
		} else {
			log.Printf("Cannot reconnect to %s: %s", GetAlias(nodeId), err)
			return false
		}
	}

	skipTor := true

try_to_connect:
	for _, addr := range addresses {
		if skipTor && strings.Contains(addr.Addr, ".onion") {
			continue
		}
		_, err := client.ConnectPeer(ctx, &lnrpc.ConnectPeerRequest{
			Addr: &lnrpc.LightningAddress{
				Pubkey: nodeId,
				Host:   addr.Addr,
			},
			Perm:    false,
			Timeout: 10,
		})
		if err == nil {
			return true
		}
	}

	if skipTor {
		skipTor = false
		goto try_to_connect
	} else {
		log.Println("Failed to reconnect to", GetAlias(nodeId))
	}

	return false
}

// spendable, receivable, mapped by channelId
func FetchChannelLimits(client lnrpc.LightningClient) (spendable map[uint64]uint64, receivable map[uint64]uint64, err error) {
	res, err := client.ListChannels(context.Background(), &lnrpc.ListChannelsRequest{
		ActiveOnly:   false,
		InactiveOnly: false,
		PublicOnly:   false,
		PrivateOnly:  false,
	})

	if err != nil {
		return
	}

	spendable = make(map[uint64]uint64)
	receivable = make(map[uint64]uint64)

	for _, ch := range res.Channels {
		spendable[ch.ChanId] = min(uint64(ch.GetLocalBalance()-int64(ch.GetLocalConstraints().GetChanReserveSat())), ch.GetLocalConstraints().GetMaxPendingAmtMsat()/1000)
		receivable[ch.ChanId] = min(uint64(ch.GetRemoteBalance()-int64(ch.GetRemoteConstraints().GetChanReserveSat())), ch.GetRemoteConstraints().GetMaxPendingAmtMsat()/1000)
	}

	return
}
