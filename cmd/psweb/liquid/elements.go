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
	"peerswap-web/cmd/psweb/ln"

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
		Amount:                toBitcoin(amountSats),
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

func GetPeginAddress(address *PeginAddress) error {

	if config.Config.Chain == "testnet" {
		// to not waste testnet sats where pegin is not implemented
		// return new P2TR address in our own bitcoin wallet
		addr, err := ln.NewAddress()
		if err != nil {
			return err
		}
		address.ClaimScript = "peg-in is not implemented on testnet"
		address.MainChainAddress = addr
		return nil
	}

	client := ElementsClient()
	service := &Elements{client}
	wallet := config.Config.ElementsWallet
	params := &[]string{}

	r, err := service.client.call("getpeginaddress", params, "/wallet/"+wallet)
	if err = handleError(err, &r); err != nil {
		log.Printf("getpeginaddress: %v", err)
		return err
	}

	err = json.Unmarshal([]byte(r.Result), &address)
	if err != nil {
		log.Printf("getpeginaddress unmarshall: %v", err)
		return err
	}

	return nil
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

func toBitcoin(amountSats uint64) float64 {
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

// identifies if this version of Elements Core supports discounted vSize
func HasDiscountedvSize() bool {
	client := ElementsClient()
	service := &Elements{client}
	wallet := config.Config.ElementsWallet
	params := &[]string{}

	r, err := service.client.call("getnetworkinfo", params, "/wallet/"+wallet)
	if err = handleError(err, &r); err != nil {
		log.Printf("getnetworkinfo: %v", err)
		return false
	}

	var response map[string]interface{}

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("getnetworkinfo unmarshall: %v", err)
		return false
	}

	return response["version"].(float64) >= 230202
}

func CreateClaimPSBT(peginTxId string,
	peginVout uint,
	peginRawTx string,
	peginTxoutProof string,
	peginClaimScript string,
	peginAmount uint64,
	liquidAddress string,
	peginTxId2 string,
	peginVout2 uint,
	peginRawTx2 string,
	peginTxoutProof2 string,
	peginClaimScript2 string,
	peginAmount2 uint64,
	liquidAddress2 string,
	fee uint64) (string, error) {

	client := ElementsClient()
	service := &Elements{client}

	// Create the inputs array
	inputs := []map[string]interface{}{
		{
			"txid":               peginTxId,
			"vout":               peginVout,
			"pegin_bitcoin_tx":   peginRawTx,
			"pegin_txout_proof":  peginTxoutProof,
			"pegin_claim_script": peginClaimScript,
		},
		{
			"txid":               peginTxId2,
			"vout":               peginVout2,
			"pegin_bitcoin_tx":   peginRawTx2,
			"pegin_txout_proof":  peginTxoutProof2,
			"pegin_claim_script": peginClaimScript2,
		},
	}

	// Create the outputs array
	outputs := []map[string]interface{}{
		{
			liquidAddress:   toBitcoin(peginAmount),
			"blinder_index": 0,
		},
		{
			liquidAddress2:  toBitcoin(peginAmount2),
			"blinder_index": 1,
		},
		{
			"fee": toBitcoin(fee),
		},
	}

	// Combine inputs and outputs into the parameters array
	params := []interface{}{inputs, outputs}

	r, err := service.client.call("createpsbt", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("Failed to create raw transaction: %v", err)
		return "", err
	}

	var response string

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("CreateClaimRawTx unmarshall: %v", err)
		return "", err
	}

	return response, nil
}

func ProcessPSBT(base64psbt, wallet string) (string, bool, error) {

	client := ElementsClient()
	service := &Elements{client}

	params := []interface{}{base64psbt}

	r, err := service.client.call("walletprocesspsbt", params, "/wallet/"+wallet)
	if err = handleError(err, &r); err != nil {
		log.Printf("Failed to process PSBT: %v", err)
		return "", false, err
	}

	var response map[string]interface{}

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("ProcessPSBT unmarshall: %v", err)
		return "", false, err
	}

	return response["psbt"].(string), response["complete"].(bool), nil
}

func CombinePSBT(psbt []string) (string, error) {

	client := ElementsClient()
	service := &Elements{client}

	params := []interface{}{psbt}

	r, err := service.client.call("combinepsbt", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("Failed to combine PSBT: %v", err)
		return "", err
	}

	var response string

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("CombinePSBT unmarshall: %v", err)
		return "", err
	}

	return response, nil
}

func FinalizePSBT(psbt string) (string, bool, error) {

	client := ElementsClient()
	service := &Elements{client}

	params := []interface{}{psbt}

	r, err := service.client.call("finalizepsbt", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("Failed to finalize PSBT: %v", err)
		return "", false, err
	}

	var response map[string]interface{}

	err = json.Unmarshal([]byte(r.Result), &response)
	if err != nil {
		log.Printf("FinalizePSBT unmarshall: %v", err)
		return "", false, err
	}

	if response["complete"].(bool) {
		return response["hex"].(string), true, nil
	}

	return response["psbt"].(string), false, nil
}
