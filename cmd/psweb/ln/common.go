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

func toSats(amount float64) int64 {
	return int64(float64(100000000) * amount)
}
