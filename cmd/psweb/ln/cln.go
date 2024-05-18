//go:build cln

package ln

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"peerswap-web/cmd/psweb/bitcoin"
	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/internet"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/elementsproject/glightning/glightning"
	"github.com/elementsproject/peerswap/peerswaprpc"

	"github.com/lightningnetwork/lnd/zpay32"
)

const (
	Implementation = "CLN"
	fileRPC        = "lightning-rpc"
)

type Forwarding struct {
	CreatedIndex uint64  `json:"created_index"`
	InChannel    string  `json:"in_channel"`
	OutChannel   string  `json:"out_channel"`
	OutMsat      uint64  `json:"out_msat"`
	FeeMsat      uint64  `json:"fee_msat"`
	ResolvedTime float64 `json:"resolved_time"`
}

var (
	// arrays mapped per channel
	forwardsIn        = make(map[uint64][]Forwarding)
	forwardsOut       = make(map[uint64][]Forwarding)
	forwardsLastIndex uint64
)

func GetClient() (*glightning.Lightning, func(), error) {
	lightning := glightning.NewLightning()
	err := lightning.StartUp(fileRPC, config.Config.RpcHost)
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() {
		//lightning.Shutdown()
	}

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

// returns number of confirmations and whether the tx can be fee bumped
func GetTxConfirmations(client *glightning.Lightning, txid string) (int32, bool) {
	res, err := client.GetInfo()
	if err != nil {
		log.Println("GetInfo:", err)
		return 0, true
	}

	tip := int32(res.Blockheight)

	height := internet.GetTxHeight(txid)

	if height == 0 {
		// mempool api error, use bitcoin core
		var result bitcoin.Transaction
		_, err = bitcoin.GetRawTransaction(txid, &result)
		if err != nil {
			return -1, true // signal tx not found
		}

		return result.Confirmations, true
	}
	return tip - height, true
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

func BumpPeginFee(newFeeRate uint64) (*SentResult, error) {

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
		sendAll)
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

// utxos: ["txid:index", ....]
func SendCoinsWithUtxos(utxos *[]string, addr string, amount int64, feeRate uint64, subtractFeeFromAmount bool) (*SentResult, error) {
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
	multiplier := uint64(1000)
	if !subtractFeeFromAmount && config.Config.Chain == "mainnet" {
		multiplier = 935 // better sets fee rate for peg-in tx with change
	}

	res, err := client.WithdrawWithUtxos(
		addr,
		&glightning.Sat{
			Value:   uint64(amount),
			SendAll: subtractFeeFromAmount,
		},
		&glightning.FeeRate{
			Rate: uint(feeRate * multiplier),
		},
		&minConf,
		inputs)

	if err != nil {
		log.Println("WithdrawWithUtxos:", err)
		return nil, err
	}

	amountSent := amount
	if subtractFeeFromAmount {
		decodedTx, err := bitcoin.DecodeRawTransaction(res.Tx)
		if err == nil {
			if len(decodedTx.Vout) == 1 {
				amountSent = toSats(decodedTx.Vout[0].Value)
			}
		}
	}

	result := SentResult{
		RawHex:    res.Tx,
		TxId:      res.TxId,
		AmountSat: amountSent,
	}

	return &result, nil
}

// always true for c-lightning
func CanRBF() bool {
	return true
}

type ListForwardsRequest struct {
	Status string `json:"status"`
	Index  string `json:"index"`
	Start  uint64 `json:"start"`
}

func (r *ListForwardsRequest) Name() string {
	return "listforwards"
}

// cache routing history per channel from cln
func CacheForwards() {
	// refresh history
	client, clean, err := GetClient()
	if err != nil {
		return
	}
	defer clean()

	var newForwards struct {
		Forwards []Forwarding `json:"forwards"`
	}

	start := uint64(0)
	if forwardsLastIndex > 0 {
		// continue from the last index + 1
		start = forwardsLastIndex + 1
	}

	// get incremental history
	client.Request(&ListForwardsRequest{
		Status: "settled",
		Index:  "created",
		Start:  start,
	}, &newForwards)

	n := len(newForwards.Forwards)
	if n > 0 {
		forwardsLastIndex = newForwards.Forwards[n-1].CreatedIndex
		for _, f := range newForwards.Forwards {
			chIn := ConvertClnToLndChannelId(f.InChannel)
			chOut := ConvertClnToLndChannelId(f.OutChannel)
			forwardsIn[chIn] = append(forwardsIn[chIn], f)
			forwardsOut[chOut] = append(forwardsOut[chOut], f)
		}
	}
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
		if e.ResolvedTime > timestamp6m {
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
		if e.ResolvedTime > timestamp6m {
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
}

// HTLC represents the structure of a single HTLC entry
type HTLC struct {
	ShortChannelID string `json:"short_channel_id"`
	ID             int    `json:"id"`
	Expiry         int    `json:"expiry"`
	Direction      string `json:"direction"`
	AmountMsat     uint64 `json:"amount_msat"`
	PaymentHash    string `json:"payment_hash"`
	State          string `json:"state"`
}

type Invoice struct {
	Label              string `json:"label"`
	Status             string `json:"status"`
	AmountReceivedMsat uint64 `json:"amount_received_msat,omitempty"`
	PaidAt             uint64 `json:"paid_at,omitempty"`
	PaymentPreimage    string `json:"payment_preimage,omitempty"`
	CreatedIndex       uint64 `json:"created_index"`
}

type ListInvoicesRequest struct {
	PaymentHash string `json:"payment_hash"`
}

func (r ListInvoicesRequest) Name() string {
	return "listinvoices"
}

type ListInvoicesResponse struct {
	Invoices []Invoice `json:"invoices"`
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
		paidOutMsat  uint64
		invoicedMsat uint64
		costMsat     uint64
	)

	channelId := ConvertLndToClnChannelId(lndChannelId)

	client, clean, err := GetClient()
	if err != nil {
		return &result
	}
	defer clean()

	var harnessNetParams = &chaincfg.TestNet3Params
	if config.Config.Chain == "mainnet" {
		harnessNetParams = &chaincfg.MainNetParams
	}

	var res ListHtlcsResponse

	err = client.Request(&ListHtlcsRequest{
		ChannelId: channelId,
	}, &res)
	if err != nil {
		log.Println("ListHtlcsRequest:", err)
	}

	for _, htlc := range res.HTLCs {
		switch htlc.State {
		case "SENT_REMOVE_ACK_REVOCATION":
			// direction in, look for invoices
			var inv ListInvoicesResponse
			err := client.Request(&ListInvoicesRequest{
				PaymentHash: htlc.PaymentHash,
			}, &inv)
			if err != nil {
				continue
			}
			if inv.Invoices[0].Status == "paid" || len(inv.Invoices) == 1 {
				continue
			}
			if inv.Invoices[0].PaidAt > timeStamp {
				if len(inv.Invoices[0].Label) > 7 {
					if inv.Invoices[0].Label[:8] != "peerswap" {
						invoicedMsat += htlc.AmountMsat
					}
				}
			}

		case "RCVD_REMOVE_ACK_REVOCATION":
			// direction out, look for payments
			var pmt ListSendPaysResponse
			err := client.Request(&ListSendPaysRequest{
				PaymentHash: htlc.PaymentHash,
			}, &pmt)
			if err != nil {
				continue
			}
			if pmt.Payments[0].Status != "complete" || len(pmt.Payments) != 1 {
				continue
			}
			if pmt.Payments[0].CompletedAt > timeStamp {
				if pmt.Payments[0].Bolt11 != "" {
					// Decode the payment request
					invoice, err := zpay32.Decode(pmt.Payments[0].Bolt11, harnessNetParams)
					if err == nil {
						if invoice.Description != nil {
							if len(*invoice.Description) > 7 {
								if (*invoice.Description)[:8] == "peerswap" {
									// skip peerswap-related payments
									continue
								}
							}
						}
					}
				}
				paidOutMsat += htlc.AmountMsat
				fee := pmt.Payments[0].AmountSentMsat - pmt.Payments[0].AmountMsat
				costMsat += fee
			}
		}
	}

	timeStampF := float64(timeStamp)

	for _, e := range forwardsOut[lndChannelId] {
		if e.ResolvedTime > timeStampF {
			amountOut += e.OutMsat
			feeMsat += e.FeeMsat
		}
	}
	for _, e := range forwardsIn[lndChannelId] {
		if e.ResolvedTime > timeStampF {
			amountIn += e.OutMsat
			assistedMsat += e.FeeMsat
		}
	}

	result.RoutedOut = amountOut / 1000
	result.RoutedIn = amountIn / 1000
	result.FeeSat = feeMsat / 1000
	result.AssistedFeeSat = assistedMsat / 1000
	result.InvoicedIn = invoicedMsat / 1000
	result.PaidOut = paidOutMsat / 1000
	result.PaidCost = costMsat / 1000

	return &result
}

type ListPeerChannelsRequest struct {
	PeerId string `json:"id,omitempty"`
}

func (r ListPeerChannelsRequest) Name() string {
	return "listpeerchannels"
}

// get fees on the channel
func GetChannelInfo(client *glightning.Lightning, lndChannelId uint64, nodeId string) *ChanneInfo {
	info := new(ChanneInfo)
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
			info.FeeBase = uint64(channelMap["fee_base_msat"].(float64))
			info.FeeRate = uint64(channelMap["fee_proportional_millionths"].(float64))
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
		AddressType: "bech32",
	}, &res)
	if err != nil {
		log.Println("NewAddrRequest:", err)
		return "", err
	}

	return res.Bech32, nil
}

// Returns Lightning channels as peerswaprpc.ListPeersResponse, excluding private channels and certain nodes
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

	for _, clnPeer := range clnPeers {
		// skip excluded
		if excludeIds != nil {
			if stringIsInSlice(clnPeer.Id, *excludeIds) {
				continue
			}
		}

		// skip peers with no channels
		if len(clnPeer.Channels) == 0 {
			continue
		}

		peer := peerswaprpc.PeerSwapPeer{}
		peer.NodeId = clnPeer.Id

		for _, channel := range clnPeer.Channels {
			peer.Channels = append(peer.Channels, &peerswaprpc.PeerSwapPeerChannel{
				ChannelId:     ConvertClnToLndChannelId(channel.ChannelId),
				LocalBalance:  channel.SpendableMilliSatoshi / 1000,
				RemoteBalance: channel.ReceivableMilliSatoshi / 1000,
				Active:        clnPeer.Connected && channel.State == "CHANNELD_NORMAL",
			})
		}

		peer.AsSender = &peerswaprpc.SwapStats{}
		peer.AsReceiver = &peerswaprpc.SwapStats{}
		peers = append(peers, &peer)
	}

	list := peerswaprpc.ListPeersResponse{
		Peers: peers,
	}

	return &list, nil
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

type GetInfoRequest struct {
}

func (r GetInfoRequest) Name() string {
	return "getinfo"
}

type GetInfoResponse struct {
	Alias string `json:"alias"`
}

func GetMyAlias() string {
	if myNodeAlias == "" {
		client, clean, err := GetClient()
		if err != nil {
			log.Println("GetClient:", err)
			return ""
		}
		defer clean()

		var res GetInfoResponse

		// Send the keysend payment
		err = client.Request(&GetInfoRequest{}, &res)
		if err != nil {
			return ""
		}

		myNodeAlias = res.Alias
	}
	return myNodeAlias
}

func CachePayments() {
	//not implemented
}

func CacheInvoices() {
	//not implemented
}
