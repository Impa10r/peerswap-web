//go:build cln

package ln

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"peerswap-web/cmd/psweb/bitcoin"
	"peerswap-web/cmd/psweb/config"

	"github.com/elementsproject/glightning/glightning"
	"github.com/elementsproject/peerswap/peerswaprpc"

	_ "github.com/mattn/go-sqlite3"
)

const (
	IMPLEMENTATION = "CLN"
	RPC_FILE       = "lightning-rpc"
)

var (
	// arrays mapped per channel
	forwardsIn        = make(map[uint64][]Forwarding)
	forwardsOut       = make(map[uint64][]Forwarding)
	forwardsLastIndex uint64
	downloadComplete  bool
	// track timestamp of the last failed forward
	failedForwardTS = make(map[uint64]int64)
	lightning       *glightning.Lightning
	// cln database calls take too long, cache them
	htlcsCache    = make(map[string][]HTLC)               // by shortChannelId
	invoicesCache = make(map[string]ListInvoicesResponse) // by PaymentHash
	sendpaysCache = make(map[string]ListSendPaysResponse) // by PaymentHash
	// sqlite3 channel Id mapped to short_channel_id
	sqlShortChannelId = make(map[int]string)
	htlcStates        = []string{"SENT_ADD_HTLC", "SENT_ADD_COMMIT", "RCVD_ADD_REVOCATION", "RCVD_ADD_ACK_COMMIT", "SENT_ADD_ACK_REVOCATION", "RCVD_REMOVE_HTLC", "RCVD_REMOVE_COMMIT", "SENT_REMOVE_REVOCATION", "SENT_REMOVE_ACK_COMMIT", "RCVD_REMOVE_ACK_REVOCATION", "RCVD_ADD_HTLC", "RCVD_ADD_COMMIT", "SENT_ADD_REVOCATION", "SENT_ADD_ACK_COMMIT", "RCVD_ADD_ACK_REVOCATION", "SENT_REMOVE_HTLC", "SENT_REMOVE_COMMIT", "RCVD_REMOVE_REVOCATION", "RCVD_REMOVE_ACK_COMMIT", "SENT_REMOVE_ACK_REVOCATION"}
	// map full channel id to short channel id
	fullToShortChannelId = make(map[string]string)
)

type Forwarding struct {
	CreatedIndex uint64  `json:"created_index"`
	InChannel    string  `json:"in_channel"`
	OutChannel   string  `json:"out_channel"`
	OutMsat      uint64  `json:"out_msat"`
	FeeMsat      uint64  `json:"fee_msat"`
	ReceivedTime float64 `json:"received_time"`
	ResolvedTime float64 `json:"resolved_time"`
	Status       string  `json:"status"`
	FailCode     uint64  `json:"failcode,omitempty"`
}

type SQLResponse struct {
	Rows [][]interface{} `json:"rows"`
}

type SQLRequest struct {
	Query string `json:"query"`
}

func (r SQLRequest) Name() string {
	return "sql"
}

func GetClient() (*glightning.Lightning, func(), error) {
	if lightning == nil {
		lightning = glightning.NewLightning()
		err := lightning.StartUp(RPC_FILE, config.Config.RpcHost)
		if err != nil {
			log.Println("Cannot start glightning client")
			return nil, nil, err
		}
	}

	cleanup := func() {} // do nothing, keep connected

	return lightning, cleanup, nil
}

func ConfirmedWalletBalance(client *glightning.Lightning) int64 {
	var response map[string]interface{}

	err := client.Request(&glightning.ListFundsRequest{}, &response)
	if err != nil {
		log.Println("ListFunds:", err)
		return 0
	}

	// Total amount_msat
	var totalAmount int64

	// Iterate over outputs and add amount_msat to total
	outputs := response["outputs"].([]interface{})
	for _, output := range outputs {
		outputMap := output.(map[string]interface{})
		if outputMap["status"] == "confirmed" {
			amountMsat := outputMap["amount_msat"].(float64)
			totalAmount += int64(amountMsat / 1000)
		}
	}
	return totalAmount
}

func ListUnspent(client *glightning.Lightning, list *[]UTXO, minConfs int32) error {
	res, err := client.GetInfo()
	if err != nil {
		log.Println("GetInfo:", err)
		return err
	}

	tip := res.Blockheight

	var response map[string]interface{}
	err = client.Request(&glightning.ListFundsRequest{}, &response)
	if err != nil {
		log.Println("ListFunds:", err)
		return err
	}

	// Dereference the pointer to get the actual array
	a := *list

	// Iterate over outputs and append to a list
	outputs := response["outputs"].([]interface{})
	for _, output := range outputs {
		outputMap := output.(map[string]interface{})
		amountMsat := outputMap["amount_msat"].(float64)
		status := outputMap["status"].(string)
		confs := int64(0)
		if status == "confirmed" {
			blockHeight := outputMap["blockheight"].(float64)
			confs = int64(tip - uint(blockHeight))
		}
		if confs >= int64(minConfs) {
			a = append(a, UTXO{
				Address:       outputMap["address"].(string),
				AmountSat:     int64(amountMsat / 1000),
				Confirmations: confs,
				TxidStr:       outputMap["txid"].(string),
				OutputIndex:   uint32(outputMap["output"].(float64)),
			})
		}
	}

	// sort the table on Confirmations field
	sort.Slice(a, func(i, j int) bool {
		return a[i].Confirmations < a[j].Confirmations
	})

	// Update the array through the pointer
	*list = a

	return nil
}
func GetBlockHeight() uint32 {
	client, _, err := GetClient()
	if err != nil {
		return 0
	}

	res, err := client.GetInfo()
	if err != nil {
		log.Println("GetInfo:", err)
		return 0
	}

	return uint32(res.Blockheight)
}

// returns number of confirmations and whether the tx can be fee bumped
func GetTxConfirmations(client *glightning.Lightning, txid string) (int32, bool) {

	var tx bitcoin.Transaction
	_, err := bitcoin.GetRawTransaction(txid, &tx)
	if err != nil {
		return -1, false // signal tx not found
	}

	return tx.Confirmations, true
}

func GetAlias(nodeKey string) string {
	// not implemented, use mempool
	return ""
}

type UtxoPsbtRequest struct {
	Satoshi     string   `json:"satoshi"`
	Feerate     string   `json:"feerate"`
	StartWeight int      `json:"startweight"`
	UTXOs       []string `json:"utxos"`
	Reserve     int      `json:"reserve"`
	ReservedOk  bool     `json:"reservedok"`
}

type UtxoPsbtResponse struct {
	ChangeOutNum         int     `json:"change_outnum"`
	EstimatedFinalWeight int     `json:"estimated_final_weight"`
	ExcessMSat           float64 `json:"excess_msat"`
	FeeratePerKW         int     `json:"feerate_per_kw"`
	PSBT                 string  `json:"psbt"`
}

func (r UtxoPsbtRequest) Name() string {
	return "utxopsbt"
}

type UnreserveInputsRequest struct {
	PSBT    string `json:"psbt"`
	Reserve int    `json:"reserve"`
}

func (r UnreserveInputsRequest) Name() string {
	return "unreserveinputs"
}

type Reservation struct {
	TxID        string `json:"txid"`
	Vout        int    `json:"vout"`
	WasReserved bool   `json:"was_reserved"`
	Reserved    bool   `json:"reserved"`
}

type UnreserveInputsResponse struct {
	Reservations []Reservation `json:"reservations"`
}

func BumpPeginFee(newFeeRate float64, label string) (*SentResult, error) {

	client, clean, err := GetClient()
	if err != nil {
		log.Println("GetClient:", err)
		return nil, err
	}
	defer clean()

	tx, err := getTransaction(client, config.Config.PeginTxId)
	if err != nil {
		return nil, err
	}

	decodedTx, err := bitcoin.DecodeRawTransaction(tx.RawTx)
	if err != nil {
		return nil, err
	}

	var utxos []string

	for _, input := range decodedTx.Vin {
		vin := input.TXID + ":" + strconv.FormatUint(uint64(input.Vout), 10)
		utxos = append(utxos, vin)
	}

	var res UtxoPsbtResponse
	err = client.Request(&UtxoPsbtRequest{
		Satoshi:     "all",
		Feerate:     "3000perkb",
		StartWeight: 0,
		UTXOs:       utxos,
		Reserve:     0,
		ReservedOk:  true,
	}, &res)
	if err != nil {
		log.Println("UtxoPsbt:", err)
		return nil, err
	}

	var res2 UnreserveInputsResponse
	err = client.Request(&UnreserveInputsRequest{
		Reserve: 1000,
		PSBT:    res.PSBT,
	}, &res2)
	if err != nil {
		log.Println("UnreserveInputs:", err)
		return nil, err
	}

	sendAll := len(decodedTx.Vout) == 1

	return SendCoinsWithUtxos(
		&utxos,
		config.Config.PeginAddress,
		config.Config.PeginAmount,
		newFeeRate,
		sendAll,
		label)
}

func getTransaction(client *glightning.Lightning, txid string) (*glightning.Transaction, error) {
	txs, err := client.ListTransactions()
	if err != nil {
		log.Println("ListTransactions", err)
		return nil, err
	}

	for _, tx := range txs {
		if tx.Hash == txid {
			return &tx, nil
		}
	}

	return nil, errors.New("transaction " + txid + " not found")
}

func GetRawTransaction(client *glightning.Lightning, txid string) (string, error) {
	tx, err := getTransaction(client, txid)

	if err != nil {
		return "", err
	}

	return tx.RawTx, nil
}

type WithdrawRequest struct {
	Destination string   `json:"destination"`
	Satoshi     string   `json:"satoshi"`
	FeeRate     string   `json:"feerate,omitempty"`
	MinConf     uint16   `json:"minconf,omitempty"`
	Utxos       []string `json:"utxos,omitempty"`
}

func (r WithdrawRequest) Name() string {
	return "withdraw"
}

type WithdrawResult struct {
	Tx   string `json:"tx"`
	TxId string `json:"txid"`
	PSBT string `json:"psbt"`
}

// utxos: ["txid:index", ....]
func SendCoinsWithUtxos(utxos *[]string, addr string, amount int64, feeRate float64, subtractFeeFromAmount bool, label string) (*SentResult, error) {
	client, clean, err := GetClient()
	if err != nil {
		log.Println("GetClient:", err)
		return nil, err
	}
	defer clean()

	inputs := []*glightning.Utxo{}

	if len(*utxos) == 0 {
		inputs = nil
	} else {
		for _, i := range *utxos {
			parts := strings.Split(i, ":")
			index, err := strconv.Atoi(parts[1])
			if err != nil {
				log.Println("Invalid UTXOs:", err)
				return nil, err
			}

			inputs = append(inputs, &glightning.Utxo{
				TxId:  parts[0],
				Index: uint(index),
			})
		}
	}

	minConf := uint16(1)
	multiplier := float64(1000)
	if !subtractFeeFromAmount && config.Config.Chain == "mainnet" {
		multiplier = 935 // better sets fee rate for pegin tx with change
	}

	amountStr := fmt.Sprintf("%d", amount)
	if subtractFeeFromAmount {
		amountStr = "all"
	}

	var res WithdrawResult
	err = client.Request(&WithdrawRequest{
		Destination: addr,
		Satoshi:     amountStr,
		FeeRate:     fmt.Sprint(uint(feeRate*multiplier)) + "perkb",
		MinConf:     minConf,
		Utxos:       *utxos,
	}, &res)
	if err != nil {
		log.Println("WithdrawRequest:", err)
		return nil, err
	}

	// The returned res.Tx is UNSIGNED, ignore it and get new
	var decoded bitcoin.Transaction
	_, err = bitcoin.GetRawTransaction(res.TxId, &decoded)
	if err != nil {
		return nil, err
	}

	amountSent := amount
	if subtractFeeFromAmount && len(decoded.Vout) == 1 {
		amountSent = toSats(decoded.Vout[0].Value)
	}

	// default value if exact calculation fails
	feePaid := feeRate * float64(decoded.VSize)

	fee := int64(0)
	for _, input := range decoded.Vin {
		var decodedIn bitcoin.Transaction
		_, err = bitcoin.GetRawTransaction(input.TXID, &decodedIn)
		if err != nil {
			goto return_result
		}
		fee += toSats(decodedIn.Vout[input.Vout].Value)
	}

	for _, output := range decoded.Vout {
		fee -= toSats(output.Value)
	}

	feePaid = float64(fee)

return_result:

	result := SentResult{
		RawHex:     decoded.Hex,
		TxId:       res.TxId,
		AmountSat:  amountSent,
		ExactSatVb: math.Ceil(feePaid*1000/float64(decoded.VSize)) / 1000,
	}

	return &result, nil
}

// always true for c-lightning
func CanRBF() bool {
	return true
}

type ListForwardsRequest struct {
	Index string `json:"index"`
	Start uint64 `json:"start"`
	Limit uint64 `json:"limit"`
}

func (r *ListForwardsRequest) Name() string {
	return "listforwards"
}

// queries lightningd.sqlite3 channel_htlcs table to pull
// htlcs into htlcsCache map sorted by short channel id
func CacheHTLCs(where string) int {
	// refresh full channel id into short mapping
	client, clean, err := GetClient()
	if err != nil {
		return 0
	}
	defer clean()

	var response map[string]interface{}
	err = client.Request(&ListPeerChannelsRequest{}, &response)
	if err != nil {
		log.Println("ListPeerChannelsRequest:", err)
		return 0
	}

	// Iterate over channels to map channel ids
	channels := response["channels"].([]interface{})
	for _, channel := range channels {
		channelMap := channel.(map[string]interface{})
		if channelMap["channel_id"] != nil {
			if channelMap["short_channel_id"] != nil {
				fullToShortChannelId[channelMap["channel_id"].(string)] = channelMap["short_channel_id"].(string)
			} else {
				// private zero-conf channel
				alias := channelMap["alias"].(map[string]interface{})
				fullToShortChannelId[channelMap["channel_id"].(string)] = alias["local"].(string)
			}
		}
	}

	folder := "bitcoin"
	if config.Config.Chain == "testnet" {
		folder = "testnet"
	}

	// Open a database connection
	db, err := sql.Open("sqlite3", config.Config.LightningDir+"/"+folder+"/lightningd.sqlite3")
	if err != nil {
		log.Println("Error opening database:", err)
		return 0
	}
	defer db.Close()

	// refresh full channel Ids
	rows, err := db.Query("SELECT id, full_channel_id FROM channels")
	if err != nil {
		log.Println("Error executing query:", err)
		return 0
	}
	defer rows.Close()

	// Iterate through the results
	for rows.Next() {
		var id int
		var data []byte
		err := rows.Scan(&id, &data)
		if err != nil {
			log.Println("Error scanning row:", err)
			continue
		}

		// map sql channel id to short channel id
		shortChannelId, found := fullToShortChannelId[hex.EncodeToString(data)]
		if !found {
			// full channel ID not found, ignore channel
			continue
		}
		sqlShortChannelId[id] = shortChannelId
	}

	// Check for errors from iterating over rows
	if err = rows.Err(); err != nil {
		log.Println("Error iterating rows:", err)
	}

	numHtlcs := 0
	limit := 1000
	offset := 0

	// Get the HTLCs
	for {
		query := fmt.Sprintf("SELECT channel_id, id, cltv_expiry, msatoshi, payment_hash, hstate FROM channel_htlcs WHERE %s LIMIT %d OFFSET %d", where, limit, offset)
		rows, err = db.Query(query)
		if err != nil {
			log.Println("Error executing query:", err)
			return 0
		}

		numRows := 0
		// Iterate through the results
		for rows.Next() {
			numRows++

			var (
				htlc   HTLC
				cid    int
				hstate int
				hash   []byte
			)

			err := rows.Scan(&cid, &htlc.Id, &htlc.Expiry, &htlc.AmountMsat, &hash, &hstate)
			if err != nil {
				log.Println("Error scanning row:", err)
				return 0
			}

			scid, ok := sqlShortChannelId[cid]
			if ok {
				htlc.ShortChannelId = scid
				htlc.State = htlcStates[hstate]
				htlc.PaymentHash = hex.EncodeToString(hash)
				htlc.Direction = "in"
				if hstate > 9 {
					htlc.Direction = "out"
				}
				appendHTLC(htlc)
				numHtlcs++
			} else {
				log.Println("Short channel id is missing for SQL channel id", cid)
			}
		}

		// Check for errors from iterating over rows
		if err = rows.Err(); err != nil {
			log.Println("Error iterating rows:", err)
		}

		if numRows < limit {
			break // end of data
		}
		offset += limit
	}

	return numHtlcs
}

// cache routing history per channel from cln
func cacheForwards(client *glightning.Lightning) int {
	// refresh history
	var newForwards struct {
		Forwards []Forwarding `json:"forwards"`
	}

	totalForwards := 0

	for {
		// get incremental history
		err := client.Request(&ListForwardsRequest{
			Index: "created",
			Start: forwardsLastIndex,
			Limit: 1000,
		}, &newForwards)
		if err != nil {
			log.Println("cacheForwards:", err)
			return 0
		}

		n := len(newForwards.Forwards)
		if n > 0 {
			forwardsLastIndex = newForwards.Forwards[n-1].CreatedIndex + 1
			for _, f := range newForwards.Forwards {
				chOut := ConvertClnToLndChannelId(f.OutChannel)
				if f.Status == "settled" && f.OutMsat >= IGNORE_FORWARDS_MSAT {
					chIn := ConvertClnToLndChannelId(f.InChannel)
					forwardsIn[chIn] = append(forwardsIn[chIn], f)
					forwardsOut[chOut] = append(forwardsOut[chOut], f)
					// save for autofees
					LastForwardTS[chOut] = int64(f.ResolvedTime)
					// forget last failed attempt
					failedForwardTS[chOut] = 0
				} else {
					// catch not enough balance error
					if f.FailCode == 4103 {
						failedForwardTS[chOut] = int64(f.ReceivedTime)
					}
				}
			}
			totalForwards += n
		} else {
			break
		}
	}

	return totalForwards
}

// get routing statistics for a channel
func GetForwardingStats(lndChannelId uint64) *ForwardingStats {
	var (
		result          ForwardingStats
		amountOut7d     uint64
		amountOut30d    uint64
		amountOut6m     uint64
		amountIn7d      uint64
		amountIn30d     uint64
		amountIn6m      uint64
		feeMsat7d       uint64
		assistedMsat7d  uint64
		feeMsat30d      uint64
		assistedMsat30d uint64
		feeMsat6m       uint64
		assistedMsat6m  uint64
	)

	// historic timestamps in sec
	now := time.Now()
	timestamp7d := float64(now.AddDate(0, 0, -7).Unix())
	timestamp30d := float64(now.AddDate(0, 0, -30).Unix())
	timestamp6m := float64(now.AddDate(0, -6, 0).Unix())

	for _, e := range forwardsOut[lndChannelId] {
		if e.ResolvedTime > timestamp6m && e.OutMsat >= IGNORE_FORWARDS_MSAT {
			amountOut6m += e.OutMsat
			feeMsat6m += e.FeeMsat
			if e.ResolvedTime > timestamp30d {
				amountOut30d += e.OutMsat
				feeMsat30d += e.FeeMsat
				if e.ResolvedTime > timestamp7d {
					amountOut7d += e.OutMsat
					feeMsat7d += e.FeeMsat
				}
			}
		}
	}
	for _, e := range forwardsIn[lndChannelId] {
		if e.ResolvedTime > timestamp6m && e.OutMsat >= IGNORE_FORWARDS_MSAT {
			amountIn6m += e.OutMsat
			assistedMsat6m += e.FeeMsat
			if e.ResolvedTime > timestamp30d {
				amountIn30d += e.OutMsat
				assistedMsat30d += e.FeeMsat
				if e.ResolvedTime > timestamp7d {
					amountIn7d += e.OutMsat
					assistedMsat7d += e.FeeMsat
				}
			}
		}
	}

	result.AmountOut7d = amountOut7d / 1000
	result.AmountOut30d = amountOut30d / 1000
	result.AmountOut6m = amountOut6m / 1000
	result.AmountIn7d = amountIn7d / 1000
	result.AmountIn30d = amountIn30d / 1000
	result.AmountIn6m = amountIn6m / 1000

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

// Payment represents the structure of the payment data
type Payment struct {
	Status         string `json:"status"`
	CompletedAt    uint64 `json:"completed_at"`
	AmountMsat     uint64 `json:"amount_msat"`
	AmountSentMsat uint64 `json:"amount_sent_msat"`
	Bolt11         string `json:"bolt11"`
	Destination    string `json:"destination"`
}

// HTLC represents the structure of a single HTLC entry
type HTLC struct {
	ShortChannelId string `json:"short_channel_id"`
	Id             uint64 `json:"id"`
	Expiry         uint64 `json:"expiry"`
	Direction      string `json:"direction"`
	AmountMsat     uint64 `json:"amount_msat"`
	PaymentHash    string `json:"payment_hash"`
	State          string `json:"state"`
}

type ListInvoicesRequest struct {
	Label       string `json:"label,omitempty"`
	PaymentHash string `json:"payment_hash,omitempty"`
}

type ListInvoicesResponse struct {
	Invoices []*glightning.Invoice `json:"invoices"`
}

func (r ListInvoicesRequest) Name() string {
	return "listinvoices"
}

type ListSendPaysRequest struct {
	PaymentHash string `json:"payment_hash"`
}

func (r ListSendPaysRequest) Name() string {
	return "listsendpays"
}

type ListSendPaysResponse struct {
	Payments []Payment `json:"payments"`
}

type ListHtlcsRequest struct {
	ChannelId string `json:"id,omitempty"`
}

func (r ListHtlcsRequest) Name() string {
	return "listhtlcs"
}

type ListHtlcsResponse struct {
	HTLCs []HTLC `json:"htlcs"`
}

// flow stats for a channel since timestamp
func GetChannelStats(lndChannelId uint64, timeStamp uint64) *ChannelStats {
	var (
		result       ChannelStats
		amountOut    uint64
		amountIn     uint64
		feeMsat      uint64
		assistedMsat uint64
	)

	channelId := ConvertLndToClnChannelId(lndChannelId)

	client, clean, err := GetClient()
	if err != nil {
		log.Println("GetClient:", err)
		return &result
	}
	defer clean()

	fetchPaymentsStats(client, timeStamp, channelId, &result)
	cacheForwards(client)

	timeStampF := float64(timeStamp)

	for _, e := range forwardsOut[lndChannelId] {
		if e.ResolvedTime > timeStampF && e.OutMsat >= IGNORE_FORWARDS_MSAT {
			amountOut += e.OutMsat
			feeMsat += e.FeeMsat
		}
	}
	for _, e := range forwardsIn[lndChannelId] {
		if e.ResolvedTime > timeStampF && e.OutMsat >= IGNORE_FORWARDS_MSAT {
			amountIn += e.OutMsat
			assistedMsat += e.FeeMsat
		}
	}

	result.RoutedOut = amountOut / 1000
	result.RoutedIn = amountIn / 1000
	result.FeeSat = feeMsat / 1000
	result.AssistedFeeSat = assistedMsat / 1000

	return &result
}

type ListPeerChannelsRequest struct {
	PeerId string `json:"id,omitempty"`
}

func (r ListPeerChannelsRequest) Name() string {
	return "listpeerchannels"
}

// get fees for the channel
func GetChannelInfo(client *glightning.Lightning, lndChannelId uint64, nodeId string) *ChanneInfo {
	info := new(ChanneInfo)

	info.ChannelId = lndChannelId

	channelId := ConvertLndToClnChannelId(lndChannelId)

	var response map[string]interface{}

	err := client.Request(&ListPeerChannelsRequest{
		PeerId: nodeId,
	}, &response)
	if err != nil {
		log.Println(err)
		return info
	}

	// Iterate over channels to find ours
	channels := response["channels"].([]interface{})
	for _, channel := range channels {
		channelMap := channel.(map[string]interface{})
		if channelMap["short_channel_id"].(string) == channelId {
			info.FeeBase = int64(channelMap["fee_base_msat"].(float64))
			info.FeeRate = int64(channelMap["fee_proportional_millionths"].(float64))
			updates := channelMap["updates"].(map[string]interface{})
			local := updates["local"].(map[string]interface{})
			remote := updates["remote"].(map[string]interface{})
			info.PeerFeeBase = int64(remote["fee_base_msat"].(float64))
			info.PeerFeeRate = int64(remote["fee_proportional_millionths"].(float64))
			info.PeerMinHtlc = msatToSatUp(uint64(remote["htlc_minimum_msat"].(float64)))
			info.PeerMaxHtlc = uint64(remote["htlc_maximum_msat"].(float64)) / 1000
			info.OurMaxHtlc = uint64(local["htlc_maximum_msat"].(float64)) / 1000
			info.OurMinHtlc = msatToSatUp(uint64(local["htlc_minimum_msat"].(float64)))
			info.Capacity = uint64(channelMap["total_msat"].(float64) / 1000)
			break
		}
	}
	return info
}

func NewAddress() (string, error) {
	client, clean, err := GetClient()
	if err != nil {
		log.Println("GetClient:", err)
		return "", err
	}
	defer clean()

	var res struct {
		Bech32     string `json:"bech32"`
		P2SHSegwit string `json:"p2sh-segwit"`
		Taproot    string `json:"p2tr"`
	}

	err = client.Request(&glightning.NewAddrRequest{
		AddressType: "p2tr",
	}, &res)
	if err != nil {
		log.Println("NewAddrRequest:", err)
		return "", err
	}

	return res.Taproot, nil
}

// Returns Lightning channels as peerswaprpc.ListPeersResponse and certain nodes
func ListPeers(client *glightning.Lightning, peerId string, excludeIds *[]string) (*peerswaprpc.ListPeersResponse, error) {
	var clnPeers []*glightning.Peer
	var err error

	if peerId == "" {
		clnPeers, err = client.ListPeers()
		if err != nil {
			return nil, err
		}
	} else {
		peer, err := client.GetPeer(peerId)
		if err != nil {
			return nil, err
		}
		clnPeers = append(clnPeers, peer)

	}

	var peers []*peerswaprpc.PeerSwapPeer

	for i, clnPeer := range clnPeers {
		// skip excluded
		if excludeIds != nil {
			if stringIsInSlice(clnPeer.Id, *excludeIds) {
				continue
			}
		}

		channels, err := client.ListPeerChannels(clnPeer.Id)
		if err != nil {
			return nil, err
		}
		// add channels
		clnPeers[i].Channels = channels

		peer := peerswaprpc.PeerSwapPeer{}
		peer.NodeId = clnPeer.Id

		for _, channel := range clnPeer.Channels {
			peer.Channels = append(peer.Channels, &peerswaprpc.PeerSwapPeerChannel{
				ChannelId:     ConvertClnToLndChannelId(channel.ShortChannelId),
				LocalBalance:  channel.ToUsMsat.MSat() / 1000,
				RemoteBalance: (channel.TotalMsat.MSat() - channel.ToUsMsat.MSat()) / 1000,
				Active:        clnPeer.Connected && channel.State == "CHANNELD_NORMAL",
			})
		}

		peer.AsSender = &peerswaprpc.SwapStats{}
		peer.AsReceiver = &peerswaprpc.SwapStats{}
		peers = append(peers, &peer)
	}

	if peerId != "" && len(peers) != 1 {
		// none found
		return nil, errors.New("Peer " + peerId + " not found")
	}

	return &peerswaprpc.ListPeersResponse{
		Peers: peers,
	}, nil
}

type KeySendRequest struct {
	Destination string            `json:"destination"`
	AmountMsat  int64             `json:"amount_msat"`
	Tlvs        map[string]string `json:"extratlvs"`
}

func (r KeySendRequest) Name() string {
	return "keysend"
}

type KeySendResponse struct {
	Message string `json:"message"`
	Status  string `json:"status"`
}

func SendKeysendMessage(destPubkey string, amountSats int64, message string) error {
	client, clean, err := GetClient()
	if err != nil {
		log.Println("GetClient:", err)
		return err
	}
	defer clean()

	var res KeySendResponse

	// Send the keysend payment
	err = client.Request(&KeySendRequest{
		Destination: destPubkey,
		AmountMsat:  amountSats * 1000,
		Tlvs: map[string]string{
			"34349334": hex.EncodeToString([]byte(message)),
		},
	}, &res)

	if err != nil {
		return err
	}

	if res.Status != "complete" {
		return fmt.Errorf("error sending kensend: %v", res.Message)
	}

	return nil
}

// download all htlcs and forwards
// return false if lightning did not start yet
func DownloadAll() bool {

	if downloadComplete {
		// only run once
		return true
	}

	client, clean, err := GetClient()
	if err != nil {
		return false
	}
	defer clean()

	// cache my node Id and alias
	resp, err := client.GetInfo()
	if err != nil {
		return false // lightning not ready yet
	}
	MyNodeId = resp.Id
	MyNodeAlias = resp.Alias

	// benchmark time
	start := time.Now()

	blockHeight := GetBlockHeight()
	if blockHeight == 0 {
		return false
	}

	// look back 6 months only
	where := fmt.Sprintf("cltv_expiry > %d", blockHeight-26_352)
	numHtlcs := CacheHTLCs(where)

	downloadComplete = true

	/*
		duration := time.Since(start)
		log.Printf("SQL took %v to execute", duration)

		// benchmark time
		start = time.Now()

		var res ListHtlcsResponse
		// cache all HTLCs
		err = client.Request(&ListHtlcsRequest{}, &res)
		if err != nil {
			// CLN not ready
			return false
		}

		// sort by channel
		for _, htlc := range res.HTLCs {
			appendHTLC(htlc)
		}
	*/

	peers, err := ListPeers(client, "", nil)
	if err != nil {
		return false
	}

	// last 6 months
	var stats ChannelStats
	timestamp := uint64(time.Now().AddDate(0, -6, 0).Unix())

	for _, peer := range peers.Peers {
		for _, channel := range peer.Channels {
			if channel.ChannelId > 0 {
				channelId := ConvertLndToClnChannelId(channel.ChannelId)
				// cache peerswap costs
				fetchPaymentsStats(client, timestamp, channelId, &stats)
			}
		}
	}

	duration := time.Since(start)
	if numHtlcs > 0 {
		log.Printf("Cached %d HTLCs in %v", numHtlcs, duration)
	}

	start = time.Now()
	forwards := cacheForwards(client)
	if forwards > 0 {
		duration = time.Since(start)
		log.Printf("Cached %d forwards in %v", forwards, duration)
	}

	return true
}

func appendHTLC(htlc HTLC) {
	// ignore unsettled
	if htlc.State != "SENT_REMOVE_ACK_REVOCATION" && htlc.State != "RCVD_REMOVE_ACK_REVOCATION" {
		return
	}
	htlcsCache[htlc.ShortChannelId] = append(htlcsCache[htlc.ShortChannelId], htlc)
}

func GetInvoice(client *glightning.Lightning, request *ListInvoicesRequest) (ListInvoicesResponse, error) {
	inv, ok := invoicesCache[request.PaymentHash]
	if !ok { // fetch from cln
		err := client.Request(request, &inv)
		if err != nil {
			return inv, err
		}
		// cache it
		invoicesCache[request.PaymentHash] = inv
	}
	return inv, nil
}

// get statistics for a channel since the timestamp
// also caches swap rebates
func fetchPaymentsStats(client *glightning.Lightning, timeStamp uint64, channelId string, result *ChannelStats) {
	var (
		paidOutMsat       uint64
		paidCostMsat      uint64
		invoicedInMsat    uint64
		rebalancedInMsat  uint64
		rebalancedOutMsat uint64
		rebalanceCostMsat uint64
	)

	for _, htlc := range htlcsCache[channelId] {
		switch htlc.State {
		case "SENT_REMOVE_ACK_REVOCATION":
			// direction in, look for invoices
			inv, err := GetInvoice(client, &ListInvoicesRequest{
				PaymentHash: htlc.PaymentHash,
			})
			if err != nil {
				continue
			}

			if len(inv.Invoices) > 0 {
				for _, i := range inv.Invoices {
					if i.Status == "paid" && i.PaidAt > timeStamp {
						amtMsat := i.MilliSatoshiReceived.MSat()
						if !processInvoice(i.Description, int64(amtMsat)) {
							// only account for non-peerswap related invoices
							invoicedInMsat += amtMsat
						}
					}
				}
			} else {
				// no invoices
				// can be a rebalance in, check timestamp and record the stats
				pmt, ok := sendpaysCache[htlc.PaymentHash]
				if !ok { // fetch from cln
					err := client.Request(&ListSendPaysRequest{
						PaymentHash: htlc.PaymentHash,
					}, &pmt)
					if err != nil {
						continue
					}
					// cache it
					sendpaysCache[htlc.PaymentHash] = pmt
				}

				for _, p := range pmt.Payments {
					if p.Status == "complete" && p.CompletedAt > timeStamp {
						rebalancedInMsat += p.AmountMsat
						rebalanceCostMsat += p.AmountSentMsat - p.AmountMsat
					}
				}
			}

		case "RCVD_REMOVE_ACK_REVOCATION":
			// direction out, look for payments
			pmt, ok := sendpaysCache[htlc.PaymentHash]
			if !ok { // fetch from cln
				err := client.Request(&ListSendPaysRequest{
					PaymentHash: htlc.PaymentHash,
				}, &pmt)
				if err != nil {
					continue
				}
				// cache it
				sendpaysCache[htlc.PaymentHash] = pmt
			}

			for _, p := range pmt.Payments {
				if p.Status == "complete" && p.CompletedAt > timeStamp {
					if !DecodeAndProcessInvoice(p.Bolt11, int64(htlc.AmountMsat)) {
						// can be a rebalance out
						if p.Destination == MyNodeId {
							rebalancedOutMsat += p.AmountSentMsat
						} else {
							// some other payment like keysend
							paidOutMsat += p.AmountSentMsat
							paidCostMsat += p.AmountSentMsat - p.AmountMsat
						}
					}
				}
			}
		}
	}

	result.PaidOut = paidOutMsat / 1000
	result.PaidCost = paidCostMsat / 1000
	result.InvoicedIn = invoicedInMsat / 1000
	result.RebalanceCost = rebalanceCostMsat / 1000
	result.RebalanceIn = rebalancedInMsat / 1000
	result.RebalanceOut = rebalancedOutMsat / 1000
}

// Estimate sat/vB fee
func EstimateFee() float64 {
	client, clean, err := GetClient()
	if err != nil {
		return 0
	}
	defer clean()

	res, err := client.FeeRates(glightning.PerKb)
	if err != nil {
		return 0
	}

	return math.Round(float64(res.Details.Urgent) / 1000)
}

// get fees for all channels by filling the maps [channelId]
func FeeReport(client *glightning.Lightning, outboundFeeRates map[uint64]int64, inboundFeeRates map[uint64]int64) error {
	var response map[string]interface{}

	err := client.Request(&ListPeerChannelsRequest{}, &response)
	if err != nil {
		log.Println(err)
		return err
	}

	// Iterate over channels to get fees
	channels := response["channels"].([]interface{})
	for _, channel := range channels {
		channelMap := channel.(map[string]interface{})
		if channelMap["short_channel_id"] != nil {
			channelId := ConvertClnToLndChannelId(channelMap["short_channel_id"].(string))
			outboundFeeRates[channelId] = int64(channelMap["fee_proportional_millionths"].(float64))
		}
	}
	return nil
}

type SetChannelRequest struct {
	Id          string `json:"id"`
	BaseMsat    int64  `json:"feebase,omitempty"`
	FeePPM      int64  `json:"feeppm,omitempty"`
	HtlcMinMsat int64  `json:"htlcmin,omitempty"`
	HtlcMaxMsat int64  `json:"htlcmax,omitempty"`
}

func (r *SetChannelRequest) Name() string {
	return "setchannel"
}

// set fee rate for a channel
func SetFeeRate(peerNodeId string,
	channelId uint64,
	feeRate int64,
	inbound bool,
	isBase bool) (int, error) {

	if inbound {
		return 0, errors.New("inbound rates are not implemented")
	}

	client, cleanup, err := GetClient()
	if err != nil {
		log.Println("SetFeeRate:", err)
		return 0, err
	}
	defer cleanup()

	clnChId := ConvertLndToClnChannelId(channelId)

	var response map[string]interface{}
	err = client.Request(&ListPeerChannelsRequest{}, &response)
	if err != nil {
		return 0, err
	}

	oldRate := 0

	// Iterate over channels to get old fee
	channels := response["channels"].([]interface{})
	for _, channel := range channels {
		channelMap := channel.(map[string]interface{})
		if channelMap["short_channel_id"] != nil {
			if clnChId == channelMap["short_channel_id"].(string) {
				oldRate = int(channelMap["fee_proportional_millionths"].(float64))
				break
			}
		}
	}

	var req SetChannelRequest
	var res map[string]interface{}

	req.Id = clnChId
	if isBase {
		req.BaseMsat = feeRate
	} else {
		req.FeePPM = feeRate
	}

	if oldRate == int(feeRate) {
		return oldRate, errors.New("rate was already set")
	}

	err = client.Request(&req, &res)
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

	var req SetChannelRequest
	var res map[string]interface{}

	req.Id = ConvertLndToClnChannelId(channelId)
	if isMax {
		req.HtlcMaxMsat = htlcMsat
	} else {
		req.HtlcMinMsat = htlcMsat
	}

	err = client.Request(&req, &res)
	if err != nil {
		log.Println("SetHtlcSize:", err)
		return err
	}

	return nil
}

func HasInboundFees() bool {
	return false
}

func ApplyAutoFees() {
	if !AutoFeeEnabledAll {
		return
	}

	client, cleanup, err := GetClient()
	if err != nil {
		return
	}
	defer cleanup()

	// incrementally refresh
	cacheForwards(client)

	var response map[string]interface{}
	if client.Request(&ListPeerChannelsRequest{}, &response) != nil {
		return
	}

	// Iterate over channels to set fees
	channels := response["channels"].([]interface{})
	for _, channel := range channels {
		channelMap := channel.(map[string]interface{})

		if channelMap["state"].(string) != "CHANNELD_NORMAL" ||
			channelMap["peer_connected"].(bool) == false ||
			channelMap["short_channel_id"] == nil {
			continue
		}

		channelId := ConvertClnToLndChannelId(channelMap["short_channel_id"].(string))

		if !AutoFeeEnabled[channelId] {
			// not enabled
			continue
		}

		params := &AutoFeeDefaults
		if AutoFee[channelId] != nil {
			// channel has custom parameters
			params = AutoFee[channelId]
		}

		oldFee := int(channelMap["fee_proportional_millionths"].(float64))
		newFee := oldFee
		liqPct := int(channelMap["to_us_msat"].(float64) * 100 / channelMap["total_msat"].(float64))

		// check 10 minutes back to be sure
		if failedForwardTS[channelId] > time.Now().Add(-time.Duration(10*time.Minute)).Unix() {
			// forget failed HTLC to prevent duplicate action
			failedForwardTS[channelId] = 0

			if liqPct <= params.LowLiqPct {
				// bump fee
				newFee += params.FailedBumpPPM
			} else {
				// move threshold or do nothing
				moveLowLiqThreshold(channelId, params.FailedMoveThreshold)
				return
			}
		}

		// if no Fail Bump
		if newFee == oldFee {
			newFee = calculateAutoFee(channelId, params, liqPct, oldFee)
		}

		// set the new rate
		if newFee != oldFee {
			// check if the fee was already set
			if lastFeeIsTheSame(channelId, newFee, false) {
				continue
			}

			peerId := channelMap["peer_id"].(string)
			_, err = SetFeeRate(peerId, channelId, int64(newFee), false, false)
			if err == nil && !lastFeeIsTheSame(channelId, newFee, false) {
				// log the last change
				LogFee(channelId, oldFee, newFee, false, false)
			}
		}
	}
}

func PlotPPM(channelId uint64) *[]DataPoint {
	var plot []DataPoint

	for _, e := range forwardsOut[channelId] {
		// ignore small forwards
		if e.OutMsat >= IGNORE_FORWARDS_MSAT {
			plot = append(plot, DataPoint{
				TS:     uint64(e.ResolvedTime),
				Amount: e.OutMsat / 1000,
				Fee:    float64(e.FeeMsat) / 1000,
				PPM:    e.FeeMsat * 1_000_000 / e.OutMsat,
			})
		}
	}

	return &plot
}

// channelId == 0 means all channels
func ForwardsLog(channelId uint64, fromTS int64) *[]DataPoint {
	var log []DataPoint

	for chId := range forwardsOut {
		if channelId > 0 && channelId != chId {
			continue
		}
		for _, e := range forwardsOut[chId] {
			// ignore small forwards
			if e.OutMsat >= IGNORE_FORWARDS_MSAT && int64(e.ResolvedTime) >= fromTS {
				log = append(log, DataPoint{
					TS:        uint64(e.ResolvedTime),
					Amount:    e.OutMsat / 1000,
					Fee:       float64(e.FeeMsat) / 1000,
					PPM:       e.FeeMsat * 1_000_000 / e.OutMsat,
					ChanIdIn:  ConvertClnToLndChannelId(e.InChannel),
					ChanIdOut: chId,
				})
			}
		}
	}

	if channelId > 0 {
		for _, e := range forwardsIn[channelId] {
			// ignore small forwards
			if e.OutMsat >= IGNORE_FORWARDS_MSAT && int64(e.ResolvedTime) >= fromTS {
				log = append(log, DataPoint{
					TS:        uint64(e.ResolvedTime),
					Amount:    e.OutMsat / 1000,
					Fee:       float64(e.FeeMsat) / 1000,
					PPM:       e.FeeMsat * 1_000_000 / e.OutMsat,
					ChanIdIn:  channelId,
					ChanIdOut: ConvertClnToLndChannelId(e.OutChannel),
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

func SendCustomMessage(peerId string, message *Message) error {
	client, _, err := GetClient()
	if err != nil {
		return err
	}

	// Serialize the message using gob
	var buffer bytes.Buffer
	encoder := gob.NewEncoder(&buffer)
	if err := encoder.Encode(message); err != nil {
		return err
	}

	// Create a buffer for the final output
	data := make([]byte, 2+len(buffer.Bytes()))

	// Write the message type prefix
	binary.BigEndian.PutUint16(data[:2], uint16(MESSAGE_TYPE))

	// Copy the JSON data to the buffer
	copy(data[2:], buffer.Bytes())

	if _, err := client.SendCustomMessage(peerId, hex.EncodeToString(data)); err != nil {
		return err
	}

	// log.Printf("Sent %d bytes %s to %s", len(buffer.Bytes()), message.Memo, GetAlias(peerId))

	return nil
}
