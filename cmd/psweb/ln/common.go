package ln

import (
	"bytes"
	"encoding/gob"
	"log"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/db"
	"peerswap-web/cmd/psweb/safemap"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/zpay32"
)

const (
	// ignore forwards < 1000 sats
	// to not blow out PPM chart
	IGNORE_FORWARDS_MSAT = 1_000_000
	// one custom type for peeswap web
	MESSAGE_TYPE    = 42065
	MESSAGE_VERSION = 1
)

var (
	// lightning payments from swap out initiator to receiver
	SwapRebates = make(map[string]int64)
	MyNodeAlias string
	MyNodeId    string

	AutoFeeEnabledAll bool
	// maps to LND channel Id
	AutoFee         = make(map[uint64]*AutoFeeParams)
	AutoFeeLog      = make(map[uint64][]*AutoFeeEvent)
	AutoFeeEnabled  = make(map[uint64]bool)
	AutoFeeDefaults = AutoFeeParams{
		FailedBumpPPM:     10,
		LowLiqPct:         10,
		LowLiqRate:        1000,
		NormalRate:        300,
		ExcessPct:         75,
		ExcessRate:        50,
		InactivityDays:    7,
		InactivityDropPPM: 5,
		InactivityDropPct: 5,
		CoolOffHours:      24,
		LowLiqDiscount:    0,
	}

	// track timestamp of the last outbound forward per channel
	LastForwardTS = safemap.New[uint64, int64]()

	// received via custom messages, per peer nodeId
	LiquidBalances  = make(map[string]*BalanceInfo)
	BitcoinBalances = make(map[string]*BalanceInfo)

	// sent via custom messages
	SentLiquidBalances  = make(map[string]*BalanceInfo)
	SentBitcoinBalances = make(map[string]*BalanceInfo)

	// chain balances, upto our channel remote balance,
	// are discoverable by peer's swap out attempts
	// better broadcast them to prevent that behavior
	AdvertiseLiquidBalance  = true
	AdvertiseBitcoinBalance = true
)

type PaymentInfo struct {
	TimeStampNs    uint64
	AmountPaidMsat uint64
	FeePaidMsat    uint64
}

type UTXO struct {
	Address       string
	AmountSat     int64
	Confirmations int64
	TxidBytes     []byte
	TxidStr       string
	OutputIndex   uint32
}

type SentResult struct {
	RawHex     string
	AmountSat  int64
	TxId       string
	ExactSatVb float64
}

type ForwardingStats struct {
	ChannelId         uint64
	AmountOut7d       uint64
	AmountIn7d        uint64
	FeeSat7d          uint64
	AssistedFeeSat7d  uint64
	FeePPM7d          uint64
	AssistedPPM7d     uint64
	AmountOut30d      uint64
	AmountIn30d       uint64
	FeeSat30d         uint64
	AssistedFeeSat30d uint64
	FeePPM30d         uint64
	AssistedPPM30d    uint64
	AmountOut6m       uint64
	AmountIn6m        uint64
	FeeSat6m          uint64
	AssistedFeeSat6m  uint64
	FeePPM6m          uint64
	AssistedPPM6m     uint64
}

type ChannelStats struct {
	RoutedOut      uint64
	RoutedIn       uint64
	FeeSat         uint64
	AssistedFeeSat uint64
	PaidOut        uint64
	InvoicedIn     uint64
	PaidCost       uint64
	RebalanceIn    uint64
	RebalanceOut   uint64
	RebalanceCost  uint64
}

type ChanneInfo struct {
	ChannelId          uint64
	LocalBalance       uint64
	RemoteBalance      uint64
	FeeRate            int64 // PPM
	FeeBase            int64 // mSat
	InboundFeeRate     int64 // PPM
	InboundFeeBase     int64 // mSat
	Active             bool
	OurMaxHtlc         uint64
	OurMinHtlc         uint64
	PeerMaxHtlc        uint64
	PeerMinHtlc        uint64
	PeerFeeRate        int64 // PPM
	PeerFeeBase        int64 // mSat
	PeerInboundFeeRate int64 // PPM
	PeerInboundFeeBase int64 // mSat

	Capacity   uint64
	LocalPct   uint64
	AutoFeeLog string
}

type AutoFeeStatus struct {
	Alias       string
	ChannelId   uint64
	Capacity    uint64
	LocalPct    uint64
	Enabled     bool
	Rule        string
	AutoFee     *AutoFeeParams
	Custom      bool
	FeeRate     int64
	InboundRate int64
	DaysNoFlow  int
	Active      bool
}

type AutoFeeParams struct {
	// fee rate ppm increase after each "Insufficient Balance" HTLC failure
	FailedBumpPPM int
	// Move Low Liq % theshold after each 'Insufficient Balance' HTLC failure above it
	FailedMoveThreshold int
	// low local balance threshold where fee rates stay high
	LowLiqPct int
	// ppm rate when liquidity is below LowLiqPct
	LowLiqRate int
	// high local balance threshold
	ExcessPct int
	// ppm rate when liquidity is at or below ExcessPct
	NormalRate int
	// ppm rate when liquidity is above ExcessPct
	ExcessRate int
	// days of outbound inactivity to start lowering rates
	InactivityDays int
	// reduce ppm by absolute number
	InactivityDropPPM int
	// and then by a percentage
	InactivityDropPct int
	// hours to wait before reducing the fee rate again
	CoolOffHours int
	// inbound fee (<0 = discount) when liquidity is below LowLiqPct
	LowLiqDiscount int
}

type AutoFeeEvent struct {
	TimeStamp int64
	OldRate   int
	NewRate   int
	IsInbound bool
	IsManual  bool
}

// for chart plotting and forwards log
type DataPoint struct {
	TS        uint64
	Amount    uint64
	Fee       float64
	PPM       uint64
	R         uint64
	Label     string
	ChanIdIn  uint64
	ChanIdOut uint64
	AliasIn   string
	AliasOut  string
	Inbound   bool
	Outbound  bool
	TimeAgo   string
	TimeUTC   string
}

// sent/received as GOB
type Message struct {
	// cleartext announcements
	Version   int
	Memo      string
	Asset     string
	Amount    uint64
	TimeStamp uint64
	// encrypted communications via peer relay
	Sender      string
	Destination string
	Payload     []byte
}

type BalanceInfo struct {
	Amount    uint64
	TimeStamp int64
}

func toSats(amount float64) int64 {
	return int64(math.Round(float64(100000000) * amount))
}

// convert short channel id 2568777x70x1 to LND format
func ConvertClnToLndChannelId(s string) uint64 {
	parts := strings.Split(s, "x")
	if len(parts) != 3 {
		return 0 // or handle error appropriately
	}

	var scid uint64
	for i, part := range parts {
		val, err := strconv.Atoi(part)
		if err != nil {
			return 0 // or handle error appropriately
		}
		switch i {
		case 0:
			scid |= uint64(val) << 40
		case 1:
			scid |= uint64(val) << 16
		case 2:
			scid |= uint64(val)
		}
	}
	return scid
}

func OnMyCustomMessage(nodeId string, payload []byte) {
	var msg Message
	var buffer bytes.Buffer

	// Write the byte slice into the buffer
	buffer.Write(payload)

	// Deserialize binary data
	decoder := gob.NewDecoder(&buffer)
	if err := decoder.Decode(&msg); err != nil {
		log.Println("Cannot deserialize the received message ")
		return
	}

	if msg.Version != MESSAGE_VERSION {
		return
	}

	switch msg.Memo {
	case "broadcast":
		// received broadcast of pegin status
		// msg.Asset: "pegin_started" or "pegin_ended"
		Broadcast(nodeId, &msg)

	case "unable":
		forgetPubKey(msg.Destination)

	case "process":
		// messages related to pegin claimjoin
		Process(&msg, nodeId)

	case "poll":
		// repeat invite to ClaimJoin
		shareInvite(nodeId)

		if config.Config.AllowSwapRequests {
			// repeat last
			if AdvertiseLiquidBalance && SentLiquidBalances[nodeId] != nil {
				if SendCustomMessage(nodeId, &Message{
					Version: MESSAGE_VERSION,
					Memo:    "balance",
					Asset:   "lbtc",
					Amount:  SentLiquidBalances[nodeId].Amount,
				}) == nil {
					// save timestamp
					SentLiquidBalances[nodeId].TimeStamp = time.Now().Unix()
				}
			}

			if config.Config.BitcoinSwaps && AdvertiseBitcoinBalance && SentBitcoinBalances[nodeId] != nil {
				if SendCustomMessage(nodeId, &Message{
					Version: MESSAGE_VERSION,
					Memo:    "balance",
					Asset:   "btc",
					Amount:  SentBitcoinBalances[nodeId].Amount,
				}) == nil {
					// save timestamp
					SentBitcoinBalances[nodeId].TimeStamp = time.Now().Unix()
				}
			}
		}

	case "balance":
		// received information
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

// convert LND channel id to CLN 2568777x70x1
func ConvertLndToClnChannelId(s uint64) string {
	block := strconv.FormatUint(s>>40, 10)
	tx := strconv.FormatUint((s>>16)&0xFFFFFF, 10)
	output := strconv.FormatUint(s&0xFFFF, 10)
	return block + "x" + tx + "x" + output
}

// returns true if the string is present in the array of strings
func stringIsInSlice(whatToFind string, whereToSearch []string) bool {
	for _, s := range whereToSearch {
		if s == whatToFind {
			return true
		}
	}
	return false
}

// returns the rule and whether it is custom
func AutoFeeRule(channelId uint64) (*AutoFeeParams, bool) {
	params := &AutoFeeDefaults
	isCustom := false
	if channelId > 0 && AutoFee[channelId] != nil {
		// channel has custom parameters
		params = AutoFee[channelId]
		isCustom = true
	}
	return params, isCustom
}

// returns a string representation for the rule and whether it is not default
func AutoFeeRatesSummary(channelId uint64) (string, bool) {
	params, isCustom := AutoFeeRule(channelId)

	excess := strconv.Itoa(params.ExcessRate)
	normal := strconv.Itoa(params.NormalRate)
	low := strconv.Itoa(params.LowLiqRate)
	disc := strconv.Itoa(params.LowLiqDiscount)

	summary := excess + "/" + normal + "/" + low
	if HasInboundFees() {
		summary += "/" + disc
	}
	return summary, isCustom
}

func LoadDB() {
	// load ClaimJoin variables
	loadClaimJoinDB()

	// load rebates from db
	db.Load("Swaps", "SwapRebates", &SwapRebates)

	// load auto fees from db
	db.Load("AutoFees", "AutoFeeEnabledAll", &AutoFeeEnabledAll)
	db.Load("AutoFees", "AutoFeeEnabled", &AutoFeeEnabled)
	db.Load("AutoFees", "AutoFee", &AutoFee)
	db.Load("AutoFees", "AutoFeeDefaults", &AutoFeeDefaults)

	// on or off
	db.Load("Peers", "AdvertiseLiquidBalance", &AdvertiseLiquidBalance)
	db.Load("Peers", "AdvertiseBitcoinBalance", &AdvertiseBitcoinBalance)

	// drop non-array legacy log
	var log map[uint64]interface{}
	db.Load("AutoFees", "AutoFeeLog", &log)

	if len(log) == 0 {
		return
	}

	// Use reflection to determine the type
	for _, data := range log {
		v := reflect.ValueOf(data)
		if v.Kind() == reflect.Slice {
			// Type is map[uint64][]*AutoFeeEvent, load again
			db.Load("AutoFees", "AutoFeeLog", &AutoFeeLog)
			return
		}
	}
}

func calculateAutoFee(channelId uint64, params *AutoFeeParams, liqPct int, oldFee int) int {
	newFee := oldFee
	if liqPct >= params.LowLiqPct {
		// normal or high liquidity regime, check if fee can be dropped
		lastUpdate := int64(0)
		lastLog := LastAutoFeeLog(channelId, false)
		if lastLog != nil {
			lastUpdate = lastLog.TimeStamp
		}

		// must be definitely above threshold and cool-off period passed
		if liqPct > params.LowLiqPct && lastUpdate < time.Now().Add(-time.Duration(params.CoolOffHours)*time.Hour).Unix() {
			// check the inactivity period
			if ts, ok := LastForwardTS.Read(channelId); !ok || ok && ts < time.Now().AddDate(0, 0, -params.InactivityDays).Unix() {
				// decrease the fee
				newFee -= params.InactivityDropPPM
				newFee = newFee * (100 - params.InactivityDropPct) / 100
			}
		}

		// check the floors
		if liqPct < params.ExcessPct {
			newFee = max(newFee, params.NormalRate)
		} else {
			newFee = max(newFee, params.ExcessRate)
		}
	} else {
		// liquidity is low, keep the rate at high value
		newFee = max(newFee, params.LowLiqRate)
	}

	return newFee
}

// msatToSatUp converts millisatoshis to satoshis, rounding up.
func msatToSatUp(msat uint64) uint64 {
	// Divide msat by 1000 and round up if there's any remainder.
	sat := msat / 1000
	if msat%1000 != 0 {
		sat++
	}
	return sat
}

// returns last log entry
func LastAutoFeeLog(channelId uint64, isInbound bool) *AutoFeeEvent {
	// Loop backwards through the array
	for i := len(AutoFeeLog[channelId]) - 1; i >= 0; i-- {
		if AutoFeeLog[channelId][i].IsInbound == isInbound {
			return AutoFeeLog[channelId][i]
		}
	}
	return nil
}

func LogFee(channelId uint64, oldRate int, newRate int, isInbound bool, isManual bool) {
	AutoFeeLog[channelId] = append(AutoFeeLog[channelId], &AutoFeeEvent{
		TimeStamp: time.Now().Unix(),
		OldRate:   oldRate,
		NewRate:   newRate,
		IsInbound: isInbound,
		IsManual:  isManual,
	})
	// persist to db
	db.Save("AutoFees", "AutoFeeLog", AutoFeeLog)
}

func moveLowLiqThreshold(channelId uint64, bump int) {
	if bump == 0 {
		return
	}

	if AutoFee[channelId] == nil {
		// add custom parameters
		AutoFee[channelId] = new(AutoFeeParams)
		// clone default values
		*AutoFee[channelId] = AutoFeeDefaults
	}

	// do not allow reaching high liquidity threshold
	if AutoFee[channelId].LowLiqPct+bump < AutoFee[channelId].ExcessPct {
		AutoFee[channelId].LowLiqPct += bump
		// persist to db
		db.Save("AutoFees", "AutoFee", AutoFee)
	}

}

// saves swap fee rebate if found,
// returns true if the payment is related to PeerSwap
func DecodeAndProcessInvoice(bolt11 string, valueMsat int64) bool {
	if bolt11 == "" {
		return false
	}

	// Decode the payment request
	invoice, err := zpay32.Decode(bolt11, getHarnessNetParams())

	if err == nil {
		if invoice.Description != nil {
			return processInvoice(*invoice.Description, valueMsat)
		}
	}
	return false
}

func processInvoice(memo string, valueMsat int64) bool {
	if parts := strings.Split(memo, " "); len(parts) > 4 {
		if parts[0] == "peerswap" {
			// find swap id
			if parts[2] == "fee" && len(parts[4]) > 0 {
				// save rebate payment
				saveSwapRabate(parts[4], valueMsat/1000)
			}
			// skip peerswap-related payments
			return true
		}
	}
	return false
}

func saveSwapRabate(swapId string, rebate int64) {
	_, exists := SwapRebates[swapId]
	if exists {
		// already existed
		return
	}
	// save rebate payment
	SwapRebates[swapId] = rebate
	// persist to db
	db.Save("Swaps", "SwapRebates", SwapRebates)
}

// check if the last logged fee rate is the same as newFee
func lastFeeIsTheSame(channelId uint64, newFee int, isInbound bool) bool {
	lastFee := LastAutoFeeLog(channelId, isInbound)
	if lastFee != nil {
		if newFee == lastFee.NewRate && time.Now().Unix()-lastFee.TimeStamp < 86_400 { // only care about the last 24h
			return true
		}
	}
	return false
}

func getHarnessNetParams() *chaincfg.Params {
	switch config.Config.Chain {
	case "regtest":
		return &chaincfg.RegressionNetParams
	case "testnet":
		return &chaincfg.TestNet3Params
	case "testnet4":
		return &chaincfg.TestNet4Params
	case "signet":
		return &chaincfg.SigNetParams
	case "mainnet":
		return &chaincfg.MainNetParams
	}

	log.Panicf("Chain %s is not supported!")
	return nil
}
