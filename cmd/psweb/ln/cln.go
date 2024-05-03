//go:build cln

package ln

import (
	"errors"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"peerswap-web/cmd/psweb/bitcoin"
	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/internet"

	"github.com/elementsproject/glightning/glightning"
)

const (
	Implementation = "CLN"
	fileRPC        = "lightning-rpc"
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
	if !subtractFeeFromAmount {
		multiplier = 935 // better sets fee rate for tx with change
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

	amountSent := config.Config.PeginAmount
	if subtractFeeFromAmount {
		decodedTx, err := bitcoin.DecodeRawTransaction(res.Tx)
		if err == nil && len(decodedTx.Vout) == 1 {
			amountSent = toSats(decodedTx.Vout[0].Value)
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

type Forwarding struct {
	CreatedIndex uint64  `json:"created_index"`
	InChannel    string  `json:"in_channel"`
	OutChannel   string  `json:"out_channel"`
	OutMsat      uint64  `json:"out_msat"`
	FeeMsat      uint64  `json:"fee_msat"`
	ResolvedTime float64 `json:"resolved_time"`
}

var forwards struct {
	Forwards []Forwarding `json:"forwards"`
}

// fetch routing statistics from cln
func FetchForwardingStats() {
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
	if len(forwards.Forwards) > 0 {
		// continue from the last index + 1
		start = forwards.Forwards[len(forwards.Forwards)-1].CreatedIndex + 1
	}

	// get incremental history
	client.Request(&ListForwardsRequest{
		Status: "settled",
		Index:  "created",
		Start:  start,
	}, &newForwards)

	// append to all history
	forwards.Forwards = append(forwards.Forwards, newForwards.Forwards...)
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

	channelId := ConvertLndToClnChannelId(lndChannelId)

	for _, e := range forwards.Forwards {
		if e.OutChannel == channelId {
			if e.ResolvedTime > timestamp6m {
				amountOut6m += e.OutMsat
				feeMsat6m += e.FeeMsat
				if e.ResolvedTime > timestamp30d {
					amountOut30d += e.OutMsat
					feeMsat30d += e.FeeMsat
					if e.ResolvedTime > timestamp7d {
						amountOut7d += e.OutMsat
						feeMsat7d += e.FeeMsat
						log.Println(e)
					}
				}
			}
		}
		if e.InChannel == channelId {
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

	return &result
}

// net balance change for a channel
func GetNetFlow(lndChannelId uint64, timeStamp uint64) int64 {

	netFlow := int64(0)
	channelId := ConvertLndToClnChannelId(lndChannelId)
	timeStampF := float64(timeStamp)

	for _, e := range forwards.Forwards {
		if e.InChannel == channelId {
			if e.ResolvedTime > timeStampF {
				netFlow -= int64(e.OutMsat)
			}
		}
		if e.OutChannel == channelId {
			if e.ResolvedTime > timeStampF {
				netFlow += int64(e.OutMsat)
			}
		}
	}
	return netFlow / 1000
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
