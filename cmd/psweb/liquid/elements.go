package liquid

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"peerswap-web/cmd/psweb/config"

	"github.com/alexmullins/zip"
)

// A RPCClient represents a JSON RPC client (over HTTP(s)).
type RPCClient struct {
	serverAddr string
	user       string
	passwd     string
	httpClient *http.Client
	timeout    int
}

// rpcRequest represent a RCP request
type rpcRequest struct {
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	Id      int64       `json:"id"`
	JsonRpc string      `json:"jsonrpc"`
}

// RPCErrorCode represents an error code to be used as a part of an RPCError
// which is in turn used in a JSON-RPC Response object.
//
// A specific type is used to help ensure the wrong errors aren't used.
type RPCErrorCode int

// RPCError represents an error that is used as a part of a JSON-RPC Response
// object.
type RPCError struct {
	Code    RPCErrorCode `json:"code,omitempty"`
	Message string       `json:"message,omitempty"`
}

// Guarantee RPCError satisfies the builtin error interface.
var _, _ error = RPCError{}, (*RPCError)(nil)

// Error returns a string describing the RPC error.  This satisfies the
// builtin error interface.
func (e RPCError) Error() string {
	return fmt.Sprintf("%d: %s", e.Code, e.Message)
}

type rpcResponse struct {
	Id     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Err    *RPCError       `json:"error"`
}

// doTimeoutRequest process a HTTP request with timeout
func (c *RPCClient) doTimeoutRequest(timer *time.Timer, req *http.Request) (*http.Response, error) {
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := c.httpClient.Do(req)
		done <- result{resp, err}
	}()
	// Wait for the read or the timeout
	select {
	case r := <-done:
		return r.resp, r.err
	case <-timer.C:
		return nil, errors.New("timeout reading data from server")
	}
}

// call prepare & exec the request
func (c *RPCClient) call(method string, params interface{}, uriPath string) (rr rpcResponse, err error) {
	connectTimer := time.NewTimer(time.Duration(c.timeout) * time.Second)
	rpcR := rpcRequest{method, params, time.Now().UnixNano(), "1.0"}
	payloadBuffer := &bytes.Buffer{}
	jsonEncoder := json.NewEncoder(payloadBuffer)
	err = jsonEncoder.Encode(rpcR)
	if err != nil {
		return
	}
	req, err := http.NewRequest("POST", c.serverAddr+uriPath, payloadBuffer)
	if err != nil {
		return
	}
	req.Header.Add("Content-Type", "application/json;charset=utf-8")
	req.Header.Add("Accept", "application/json")

	// Auth ?
	if len(c.user) > 0 || len(c.passwd) > 0 {
		req.SetBasicAuth(c.user, c.passwd)
	}

	resp, err := c.doTimeoutRequest(connectTimer, req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	err = json.Unmarshal(data, &rr)
	if err != nil {
		err = errors.New(string(data))
	}
	return
}

// handleError handle error returned by client.call
func handleError(err error, r *rpcResponse) error {
	if err != nil {
		return err
	}
	if r.Err != nil {
		return r.Err
	}

	return nil
}

type Elements struct {
	client *RPCClient
}

// ElementsClient returns an RpcClient
func ElementsClient() (c *RPCClient) {
	httpClient := &http.Client{}
	serverAddr := fmt.Sprintf("%s:%s", config.Config.ElementsHost, config.Config.ElementsPort)
	c = &RPCClient{
		serverAddr: serverAddr,
		user:       config.Config.ElementsUser,
		passwd:     config.Config.ElementsPass,
		httpClient: httpClient,
		timeout:    30}
	return
}

type UTXO struct {
	TxID             string  `json:"txid"`
	Vout             int     `json:"vout"`
	Address          string  `json:"address,omitempty"`
	Label            string  `json:"label,omitempty"`
	ScriptPubKey     string  `json:"scriptPubKey"`
	Amount           float64 `json:"amount"`
	AmountCommitment string  `json:"amountcommitment"`
	Asset            string  `json:"asset"`
	AssetCommitment  string  `json:"assetcommitment"`
	AmountBlinder    string  `json:"amountblinder"`
	AssetBlinder     string  `json:"assetblinder"`
	Confirmations    uint64  `json:"confirmations"`
	AncestorCount    int     `json:"ancestorcount,omitempty"`
	AncestorSize     int     `json:"ancestorsize,omitempty"`
	AncestorFees     int     `json:"ancestorfees,omitempty"`
	RedeemScript     string  `json:"redeemScript,omitempty"`
	WitnessScript    string  `json:"witnessScript,omitempty"`
	Spendable        bool    `json:"spendable"`
	Solvable         bool    `json:"solvable"`
	Reused           bool    `json:"reused,omitempty"`
	Desc             string  `json:"desc,omitempty"`
	Safe             bool    `json:"safe"`
}

func ListUnspent(outputs *[]UTXO) error {
	client := ElementsClient()
	service := &Elements{client}
	params := []string{}
	wallet := config.Config.ElementsWallet

	r, err := service.client.call("listunspent", params, "/wallet/"+wallet)
	if err = handleError(err, &r); err != nil {
		log.Printf("Elements rpc: %v", err)
		return err
	}

	// Unmarshal the JSON array into a slice of LiquidUTXO structs
	err = json.Unmarshal([]byte(r.Result), &outputs)
	if err != nil {
		return err
	}
	return nil
}

type SendParams struct {
	Address               string  `json:"address"`
	Amount                float64 `json:"amount"`
	Comment               string  `json:"comment"`
	SubtractFeeFromAmount bool    `json:"subtractfeefromamount,omitempty"`
	Replaceable           bool    `json:"replaceable,omitempty"`
	IgnoreBlindFail       bool    `json:"ignoreblindfail,omitempty"`
}

func SendToAddress(address string,
	amountSats uint64,
	comment string,
	subtractFeeFromAmount bool,
	replaceable bool,
	ignoreBlindFail bool,
) (string, error) {
	client := ElementsClient()
	service := &Elements{client}
	wallet := config.Config.ElementsWallet

	params := &SendParams{
		Address:               address,
		Amount:                ToBitcoin(amountSats),
		Comment:               comment,
		SubtractFeeFromAmount: subtractFeeFromAmount,
		Replaceable:           replaceable,
		IgnoreBlindFail:       ignoreBlindFail,
	}

	r, err := service.client.call("sendtoaddress", params, "/wallet/"+wallet)
	if err = handleError(err, &r); err != nil {
		log.Printf("Elements rpc: %v", err)
		return "", err
	}

	txid := ""
	err = json.Unmarshal([]byte(r.Result), &txid)
	if err != nil {
		return "", err
	}
	return txid, nil
}

// Backup wallet and zip it with Elements Core password
// .bak's name is equal to master blinding key
func BackupAndZip(wallet string) (string, error) {

	client := ElementsClient()
	service := &Elements{client}

	r, err := service.client.call("dumpmasterblindingkey", []string{}, "/wallet/"+wallet)
	if err = handleError(err, &r); err != nil {
		log.Printf("Elements dumpmasterblindingkey: %v", err)
		return "", err
	}

	key := ""

	// Unmarshal the JSON array into masterblindingkey
	err = json.Unmarshal([]byte(r.Result), &key)
	if err != nil {
		return "", err
	}

	fileName := key + ".bak"
	params := []string{filepath.Join(config.Config.ElementsDir, fileName)}

	r, err = service.client.call("backupwallet", params, "/wallet/"+wallet)
	if err = handleError(err, &r); err != nil {
		log.Printf("Elements backupwallet: %v", err)
		return "", err
	}

	destinationZip := time.Now().Format("2006-01-02") + "_" + wallet + ".zip"
	password := config.Config.ElementsPass
	sourceFile := filepath.Join(config.Config.ElementsDirMapped, fileName)

	// Open the file
	file, err := os.Open(sourceFile)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// Get the file size
	fileInfo, err := file.Stat()
	if err != nil {
		return "", err
	}
	fileSize := fileInfo.Size()

	// Create a byte slice with the size of the file
	contents := make([]byte, fileSize)

	// Read the file into the byte slice
	_, err = io.ReadFull(file, contents)
	if err != nil {
		return "", err
	}

	fzip, err := os.Create(filepath.Join(config.Config.DataDir, destinationZip))
	if err != nil {
		return "", err
	}
	zipw := zip.NewWriter(fzip)
	defer zipw.Close()

	w, err := zipw.Encrypt(fileName, password)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(w, bytes.NewReader(contents))
	if err != nil {
		return "", err
	}
	zipw.Flush()

	return destinationZip, nil
}

type PeginAddress struct {
	MainChainAddress string `json:"mainchain_address"`
	ClaimScript      string `json:"claim_script"`
}

func GetPeginAddress() (*PeginAddress, error) {

	client := ElementsClient()
	service := &Elements{client}
	wallet := config.Config.ElementsWallet
	params := &[]string{}

	r, err := service.client.call("getpeginaddress", params, "/wallet/"+wallet)
	if err = handleError(err, &r); err != nil {
		log.Printf("getpeginaddress: %v", err)
		return nil, err
	}

	var address PeginAddress
	err = json.Unmarshal([]byte(r.Result), &address)
	if err != nil {
		log.Printf("getpeginaddress unmarshall: %v", err)
		return nil, err
	}

	return &address, nil
}

func ClaimPegin(rawTx, proof, claimScript string) (string, error) {
	client := ElementsClient()
	service := &Elements{client}
	params := []interface{}{rawTx, proof, claimScript}
	wallet := config.Config.ElementsWallet

	r, err := service.client.call("claimpegin", params, "/wallet/"+wallet)
	if err = handleError(err, &r); err != nil {
		log.Printf("claimpegin: %v", err)
		return "", err
	}

	txid := ""
	err = json.Unmarshal([]byte(r.Result), &txid)
	if err != nil {
		log.Printf("claimpegin unmarshall: %v", err)
		return "", err
	}
	return txid, nil
}

func ToBitcoin(amountSats uint64) float64 {
	return float64(amountSats) / float64(100_000_000)
}

type MemPoolInfo struct {
	Loaded           bool    `json:"loaded"`
	Size             int     `json:"size"`
	Bytes            int     `json:"bytes"`
	Usage            int     `json:"usage"`
	TotalFee         float64 `json:"total_fee"`
	MaxMemPool       int     `json:"maxmempool"`
	MemPoolMinFee    float64 `json:"mempoolminfee"`
	MinRelayTxFee    float64 `json:"minrelaytxfee"`
	UnbroadcastCount int     `json:"unbroadcastcount"`
}

// return fee estimate in sat/vB
func EstimateFee() float64 {
	client := ElementsClient()
	service := &Elements{client}
	params := &[]string{}

	r, err := service.client.call("getmempoolinfo", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("getmempoolinfo: %v", err)
		return 0
	}

	var result MemPoolInfo

	err = json.Unmarshal([]byte(r.Result), &result)
	if err != nil {
		log.Printf("getmempoolinfo unmarshall: %v", err)
		return 0
	}

	return math.Round(result.MemPoolMinFee*100_000_000) / 1000
}

// get version of Elements Core supports discounted vSize
func GetVersion() int {
	client := ElementsClient()
	service := &Elements{client}
	wallet := config.Config.ElementsWallet
	params := &[]string{}

	r, err := service.client.call("getnetworkinfo", params, "/wallet/"+wallet)
	if err = handleError(err, &r); err != nil {
		log.Printf("Elements GetVersion error: %v", err)
		return 0
	}

	var response map[string]interface{}

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("Elements GetVersion error: %v", err)
		return 0
	}

	return int(response["version"].(float64))
}

// returns block hash
func GetBlockHash(block uint32) (string, error) {
	client := ElementsClient()
	service := &Elements{client}
	params := &[]interface{}{block}

	r, err := service.client.call("getblockhash", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("GetBlockHash: %v", err)
		return "", err
	}

	var response string

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("GetBlockHash unmarshall: %v", err)
		return "", err
	}

	return response, nil
}

func CreatePSET(params interface{}) (string, error) {

	client := ElementsClient()
	service := &Elements{client}

	r, err := service.client.call("createpsbt", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("Failed to create PSET: %v", err)
		return "", err
	}

	var response string

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("CreatePSET unmarshall: %v", err)
		return "", err
	}

	return response, nil
}

func ProcessPSET(base64psbt, wallet string) (string, bool, error) {

	client := ElementsClient()
	service := &Elements{client}

	params := []interface{}{base64psbt}

	r, err := service.client.call("walletprocesspsbt", params, "/wallet/"+wallet)
	if err = handleError(err, &r); err != nil {
		log.Printf("Failed to process PSET: %v", err)
		return "", false, err
	}

	var response map[string]interface{}

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("ProcessPSET unmarshall: %v", err)
		return "", false, err
	}

	return response["psbt"].(string), response["complete"].(bool), nil
}

func FinalizePSET(psbt string) (string, bool, error) {

	client := ElementsClient()
	service := &Elements{client}

	params := []interface{}{psbt}

	r, err := service.client.call("finalizepsbt", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("Failed to finalize PSET: %v", err)
		return "", false, err
	}

	var response map[string]interface{}

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("FinalizePSET unmarshall: %v", err)
		return "", false, err
	}

	if response["complete"].(bool) {
		return response["hex"].(string), true, nil
	}

	return response["psbt"].(string), false, nil
}

type Missing struct {
	Pubkeys []string `json:"pubkeys"`
}

type AnalyzeInput struct {
	HasUTXO bool    `json:"has_utxo"`
	IsFinal bool    `json:"is_final"`
	Next    string  `json:"next"`
	Missing Missing `json:"missing"`
}

type AnalyzeOutput struct {
	Blind  bool   `json:"blind"`
	Status string `json:"status"`
}

type AnalyzedPSET struct {
	Inputs  []AnalyzeInput  `json:"inputs"`
	Outputs []AnalyzeOutput `json:"outputs"`
	Fee     float64         `json:"fee"`
	Next    string          `json:"next"`
}

func AnalyzePSET(psbt string) (*AnalyzedPSET, error) {

	client := ElementsClient()
	service := &Elements{client}

	params := []interface{}{psbt}

	r, err := service.client.call("analyzepsbt", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("Failed to analyze PSET: %v", err)
		return nil, err
	}

	var response AnalyzedPSET

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("AnalyzePSET unmarshall: %v", err)
		return nil, err
	}

	return &response, nil
}

type DecodedScript struct {
	Asm     string `json:"asm"`
	Desc    string `json:"desc"`
	Hex     string `json:"hex"`
	Address string `json:"address,omitempty"`
	Type    string `json:"type"`
}

type DecodedOutput struct {
	Amount         float64       `json:"amount"`
	Script         DecodedScript `json:"script"`
	Asset          string        `json:"asset"`
	BlindingPubKey string        `json:"blinding_pubkey,omitempty"`
	BlinderIndex   int           `json:"blinder_index,omitempty"`
	Status         string        `json:"status,omitempty"`
}

type DecodedInput struct {
	PreviousTxid       string   `json:"previous_txid"`
	PreviousVout       int      `json:"previous_vout"`
	Sequence           uint32   `json:"sequence"`
	PeginBitcoinTx     string   `json:"pegin_bitcoin_tx"`
	PeginTxoutProof    string   `json:"pegin_txout_proof"`
	PeginClaimScript   string   `json:"pegin_claim_script"`
	PeginGenesisHash   string   `json:"pegin_genesis_hash"`
	PeginValue         float64  `json:"pegin_value"`
	FinalScriptWitness []string `json:"final_scriptwitness,omitempty"`
}

type DecodedFees struct {
	Bitcoin float64 `json:"bitcoin"`
}

type DecodedPSET struct {
	GlobalXpubs      []interface{}          `json:"global_xpubs"`
	TxVersion        int                    `json:"tx_version"`
	FallbackLocktime int                    `json:"fallback_locktime"`
	InputCount       int                    `json:"input_count"`
	OutputCount      int                    `json:"output_count"`
	PsbtVersion      int                    `json:"psbt_version"`
	Proprietary      []interface{}          `json:"proprietary"`
	Fees             DecodedFees            `json:"fees"`
	Unknown          map[string]interface{} `json:"unknown"`
	Inputs           []DecodedInput         `json:"inputs"`
	Outputs          []DecodedOutput        `json:"outputs"`
}

func DecodePSET(psbt string) (*DecodedPSET, error) {

	client := ElementsClient()
	service := &Elements{client}

	params := []interface{}{psbt}

	r, err := service.client.call("decodepsbt", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("Failed to decode PSET: %v", err)
		return nil, err
	}

	var response DecodedPSET

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("DecodePSET unmarshall: %v", err)
		return nil, err
	}

	return &response, nil
}

type ScriptSig struct {
	Asm string `json:"asm"`
	Hex string `json:"hex"`
}

type TxinWitness []string
type PeginWitness []string

type Vin struct {
	Txid         string       `json:"txid"`
	Vout         int          `json:"vout"`
	ScriptSig    ScriptSig    `json:"scriptSig"`
	IsPegin      bool         `json:"is_pegin"`
	Sequence     uint32       `json:"sequence"`
	TxinWitness  TxinWitness  `json:"txinwitness"`
	PeginWitness PeginWitness `json:"pegin_witness"`
}

type ScriptPubKey struct {
	Asm     string `json:"asm"`
	Desc    string `json:"desc"`
	Hex     string `json:"hex"`
	Address string `json:"address"`
	Type    string `json:"type"`
}

type Vout struct {
	ValueMinimum         float64      `json:"value-minimum"`
	ValueMaximum         float64      `json:"value-maximum"`
	CtExponent           int          `json:"ct-exponent"`
	CtBits               int          `json:"ct-bits"`
	SurjectionProof      string       `json:"surjectionproof"`
	ValueCommitment      string       `json:"valuecommitment"`
	AssetCommitment      string       `json:"assetcommitment"`
	CommitmentNonce      string       `json:"commitmentnonce"`
	CommitmentNonceValid bool         `json:"commitmentnonce_fully_valid"`
	N                    int          `json:"n"`
	ScriptPubKey         ScriptPubKey `json:"scriptPubKey"`
	Value                float64      `json:"value,omitempty"`
	Asset                string       `json:"asset,omitempty"`
}

type Transaction struct {
	Txid          string             `json:"txid"`
	Hash          string             `json:"hash"`
	Wtxid         string             `json:"wtxid"`
	Withash       string             `json:"withash"`
	Version       int                `json:"version"`
	Size          int                `json:"size"`
	Vsize         int                `json:"vsize"`
	DiscountVsize int                `json:"discountvsize"`
	Weight        int                `json:"weight"`
	Locktime      int                `json:"locktime"`
	Vin           []Vin              `json:"vin"`
	Vout          []Vout             `json:"vout"`
	Fee           map[string]float64 `json:"fee"`
}

func DecodeRawTransaction(hexTx string) (*Transaction, error) {

	client := ElementsClient()
	service := &Elements{client}

	params := []interface{}{hexTx}

	r, err := service.client.call("decoderawtransaction", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("Failed to decode raw tx: %v", err)
		return nil, err
	}

	var response Transaction

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("DecodeRawTransaction unmarshall: %v", err)
		return nil, err
	}

	return &response, nil
}

func GetRawTransaction(txid string, result *Transaction) (string, error) {
	client := ElementsClient()
	service := &Elements{client}

	params := []interface{}{txid, result != nil}

	r, err := service.client.call("getrawtransaction", params, "")
	if err = handleError(err, &r); err != nil {
		return "", err
	}

	raw := ""
	if result == nil {
		// return raw hex
		err = json.Unmarshal([]byte(r.Result), &raw)
		if err != nil {
			log.Printf("GetRawTransaction unmarshall raw: %v", err)
			return "", err
		}
	} else {
		// decode into result
		err = json.Unmarshal([]byte(r.Result), &result)
		if err != nil {
			log.Printf("GetRawTransaction decode: %v", err)
			return "", err
		}
	}

	return raw, nil
}

func SendRawTransaction(hexTx string) (string, error) {

	client := ElementsClient()
	service := &Elements{client}

	params := []interface{}{hexTx}

	r, err := service.client.call("sendrawtransaction", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("Failed to send raw tx: %v", err)
		return "", err
	}

	var response string

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("SendRawTransaction unmarshall: %v", err)
		return "", err
	}

	return response, nil
}

type AddressInfo struct {
	Address             string   `json:"address"`
	ScriptPubKey        string   `json:"scriptPubKey"`
	IsMine              bool     `json:"ismine"`
	Solvable            bool     `json:"solvable"`
	Desc                string   `json:"desc"`
	IsWatchOnly         bool     `json:"iswatchonly"`
	IsScript            bool     `json:"isscript"`
	IsWitness           bool     `json:"iswitness"`
	WitnessVersion      int      `json:"witness_version"`
	WitnessProgram      string   `json:"witness_program"`
	Pubkey              string   `json:"pubkey"`
	Confidential        string   `json:"confidential"`
	ConfidentialKey     string   `json:"confidential_key"`
	Unconfidential      string   `json:"unconfidential"`
	IsChange            bool     `json:"ischange"`
	Timestamp           int64    `json:"timestamp"`
	HDKeyPath           string   `json:"hdkeypath"`
	HDSeedID            string   `json:"hdseedid"`
	HDMasterFingerprint string   `json:"hdmasterfingerprint"`
	Labels              []string `json:"labels"`
}

func GetAddressInfo(addr, wallet string) (*AddressInfo, error) {

	client := ElementsClient()
	service := &Elements{client}

	params := []interface{}{addr}

	r, err := service.client.call("getaddressinfo", params, "/wallet/"+wallet)
	if err = handleError(err, &r); err != nil {
		log.Printf("Failed to get address info: %v", err)
		return nil, err
	}

	var response AddressInfo

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("GetAddressInfo unmarshall: %v", err)
		return nil, err
	}

	return &response, nil
}

type BlockchainInfo struct {
	Chain                string   `json:"chain"`
	Blocks               int      `json:"blocks"`
	Headers              int      `json:"headers"`
	BestBlockHash        string   `json:"bestblockhash"`
	Time                 int64    `json:"time"`
	Mediantime           int64    `json:"mediantime"`
	VerificationProgress float64  `json:"verificationprogress"`
	InitialBlockDownload bool     `json:"initialblockdownload"`
	SizeOnDisk           int64    `json:"size_on_disk"`
	Pruned               bool     `json:"pruned"`
	CurrentParamsRoot    string   `json:"current_params_root"`
	CurrentSignblockAsm  string   `json:"current_signblock_asm"`
	CurrentSignblockHex  string   `json:"current_signblock_hex"`
	MaxBlockWitness      int      `json:"max_block_witness"`
	CurrentFedpegProgram string   `json:"current_fedpeg_program"`
	CurrentFedpegScript  string   `json:"current_fedpeg_script"`
	ExtensionSpace       []string `json:"extension_space"`
	EpochLength          int      `json:"epoch_length"`
	TotalValidEpochs     int      `json:"total_valid_epochs"`
	EpochAge             int      `json:"epoch_age"`
	Warnings             string   `json:"warnings"`
}

// returns block hash
func GetBlockchainInfo() (*BlockchainInfo, error) {
	client := ElementsClient()
	service := &Elements{client}
	params := &[]interface{}{}

	r, err := service.client.call("getblockchaininfo", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("GetBlockchainInfo: %v", err)
		return nil, err
	}

	var response BlockchainInfo

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("GetBlockchainInfo unmarshall: %v", err)
		return nil, err
	}

	return &response, nil
}

type WalletInfo struct {
	WalletName            string      `json:"walletname"`
	WalletVersion         int         `json:"walletversion"`
	Format                string      `json:"format"`
	Balance               BalanceInfo `json:"balance"`
	UnconfirmedBalance    BalanceInfo `json:"unconfirmed_balance"`
	ImmatureBalance       BalanceInfo `json:"immature_balance"`
	TxCount               int         `json:"txcount"`
	KeypoolSize           int         `json:"keypoolsize"`
	KeypoolSizeHDInternal int         `json:"keypoolsize_hd_internal"`
	PayTxFee              float64     `json:"paytxfee"`
	PrivateKeysEnabled    bool        `json:"private_keys_enabled"`
	AvoidReuse            bool        `json:"avoid_reuse"`
	Scanning              bool        `json:"scanning"`
	Descriptors           bool        `json:"descriptors"`
	ExternalSigner        bool        `json:"external_signer"`
}

type BalanceInfo struct {
	Bitcoin float64 `json:"bitcoin"`
}

// returns block hash
func GetWalletInfo(wallet string) (*WalletInfo, error) {
	client := ElementsClient()
	service := &Elements{client}
	params := &[]interface{}{}

	r, err := service.client.call("getwalletinfo", params, "/wallet/"+wallet)
	if err = handleError(err, &r); err != nil {
		log.Printf("GetWalletInfo: %v", err)
		return nil, err
	}

	var response WalletInfo

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("GetWalletInfo unmarshall: %v", err)
		return nil, err
	}

	return &response, nil
}

func GetNewAddress(label, addressType, wallet string) (string, error) {

	client := ElementsClient()
	service := &Elements{client}

	params := []interface{}{label, addressType}

	r, err := service.client.call("getnewaddress", params, "/wallet/"+wallet)
	if err = handleError(err, &r); err != nil {
		log.Printf("Failed to get new address: %v", err)
		return "", err
	}

	var response string

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("GetNewAddress unmarshall: %v", err)
		return "", err
	}

	return response, nil
}
