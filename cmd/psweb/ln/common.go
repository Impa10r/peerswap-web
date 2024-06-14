package ln

import (
	"strconv"
	"strings"
	"time"

	"peerswap-web/cmd/psweb/db"
)

type UTXO struct {
	Address       string
	AmountSat     int64
	Confirmations int64
	TxidBytes     []byte
	TxidStr       string
	OutputIndex   uint32
}

type SentResult struct {
	RawHex    string
	AmountSat int64
	TxId      string
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
}

type ChanneInfo struct {
	ChannelId       uint64
	LocalBalance    uint64
	RemoteBalance   uint64
	FeeRate         int64 // PPM
	FeeBase         int64 // mSat
	InboundFeeRate  int64 // PPM
	InboundFeeBase  int64 // mSat
	Active          bool
	OurMaxHtlcMsat  uint64
	OurMinHtlcMsat  uint64
	PeerMaxHtlcMsat uint64
	PeerMinHtlcMsat uint64
	Capacity        uint64
}

type AutoFeeStatus struct {
	Alias         string
	ChannelId     uint64
	LocalBalance  uint64
	LocalPct      uint64
	RemoteBalance uint64
	Enabled       bool
	Rates         string
	Custom        bool
}

type AutoFeeParams struct {
	// fee rate ppm increase after each "Insufficient Balance" HTLC failure
	FailedBumpPPM int
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
}

var (
	// lightning payments from swap out initiator to receiver
	SwapRebates = make(map[string]int64)
	myNodeAlias string
	myNodeId    string

	AutoFeeEnabledAll bool
	// maps to LND channel Id
	AutoFee         = make(map[uint64]*AutoFeeParams)
	AutoFeeEnabled  = make(map[uint64]bool)
	AutoFeeDefaults = AutoFeeParams{
		FailedBumpPPM:     10,
		LowLiqPct:         20,
		LowLiqRate:        2000,
		NormalRate:        300,
		ExcessPct:         70,
		ExcessRate:        50,
		InactivityDays:    14,
		InactivityDropPPM: 10,
		InactivityDropPct: 5,
		CoolOffHours:      12,
	}
)

func toSats(amount float64) int64 {
	return int64(float64(100000000) * amount)
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

// returns the rule and whether it is custom and enabled
func AutoFeeRule(channelId uint64) (*AutoFeeParams, bool) {
	params := &AutoFeeDefaults
	isCustom := false
	if AutoFee[channelId] != nil {
		// channel has custom parameters
		params = AutoFee[channelId]
		isCustom = true
	}
	return params, isCustom
}

// returns a string representation for the rule and whether it is not default
func AutoFeeRatesSummary(channelId uint64) (string, bool) {
	params, isCustom := AutoFeeRule(channelId)

	excess := strconv.FormatUint(uint64(params.ExcessRate), 10)
	normal := strconv.FormatUint(uint64(params.NormalRate), 10)
	low := strconv.FormatUint(uint64(params.LowLiqRate), 10)

	return excess + "/" + normal + "/" + low, isCustom
}

func LoadAutoFees() {
	db.Load("AutoFees", "AutoFeeEnabledAll", &AutoFeeEnabledAll)
	db.Load("AutoFees", "AutoFeeEnabled", &AutoFeeEnabled)
	db.Load("AutoFees", "AutoFee", &AutoFee)
	db.Load("AutoFees", "AutoFeeDefaults", &AutoFeeDefaults)
}

func calculateAutoFee(channelId uint64, params *AutoFeeParams, liqPct int, oldFee int, lastUpdate uint32) int {
	newFee := oldFee
	if liqPct > params.LowLiqPct {
		// normal or high liquidity regime, check if fees can be dropped
		if lastUpdate < uint32(time.Now().Add(-time.Duration(params.CoolOffHours)*time.Hour).Unix()) {
			// check the last outbound timestamp
			if lastForwardTS[channelId] < time.Now().AddDate(0, 0, -params.InactivityDays).Unix() {
				// decrease the fee
				newFee -= params.InactivityDropPPM
				newFee = newFee * (100 - params.InactivityDropPct) / 100
			}

			// check the floors
			if liqPct < params.ExcessPct {
				newFee = max(newFee, params.NormalRate)
			} else {
				newFee = max(newFee, params.ExcessRate)
			}
		}
	} else {
		// liquidity is low, floor the rate at high value
		newFee = max(newFee, params.LowLiqRate)
	}

	return newFee
}
