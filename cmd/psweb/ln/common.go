package ln

import (
	"strconv"
	"strings"
)

var myNodeAlias string

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
	LocalBalance  uint64
	RemoteBalance uint64
	FeeRate       uint64
	FeeBase       uint64
	Active        bool
}

// lighting payments from swap out initiator to receiver
var SwapRebatesMsat = make(map[string]uint64)

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
