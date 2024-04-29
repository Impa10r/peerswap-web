package ln

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
	AmountOut30d      uint64
	AmountIn30d       uint64
	FeeSat30d         uint64
	AssistedFeeSat30d uint64
	AmountOut6m       uint64
	AmountIn6m        uint64
	FeeSat6m          uint64
	AssistedFeeSat6m  uint64
}

func toSats(amount float64) int64 {
	return int64(float64(100000000) * amount)
}
