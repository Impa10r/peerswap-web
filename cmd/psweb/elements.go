package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"time"
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

// NewClient returns an RpcClient
func NewClient() (c *RPCClient) {
	host := readVariableFromPeerswapdConfig("elementsd.rpchost")
	port := readVariableFromPeerswapdConfig("elementsd.rpcport")
	user := config.ElementsUser
	passwd := config.ElementsPass

	httpClient := &http.Client{}
	serverAddr := fmt.Sprintf("%s:%s", host, port)
	c = &RPCClient{serverAddr: serverAddr, user: user, passwd: passwd, httpClient: httpClient, timeout: 5}
	return
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

// backUpWallet calls Elements RPC to save wallet file to fileName path/file
func backupWallet() (string, error) {
	wallet := readVariableFromPeerswapdConfig("elementsd.rpcwallet")
	fileName := wallet + ".bak"

	client := NewClient()
	service := &Elements{client}
	params := []string{filepath.Join(config.ElementsDir, fileName)}

	r, err := service.client.call("backupwallet", params, "/wallet/"+wallet)
	if err = handleError(err, &r); err != nil {
		log.Printf("Elements rpc: %v", err)
		return "", err
	}

	return fileName, nil
}

type LiquidUTXO struct {
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

func listUnspent(outputs *[]LiquidUTXO) error {
	client := NewClient()
	service := &Elements{client}
	params := []string{}
	wallet := readVariableFromPeerswapdConfig("elementsd.rpcwallet")

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
	SubtractFeeFromAmount bool    `json:"subtractfeefromamount"`
}

func sendToAddress(address string,
	amountSats uint64,
	subtractFeeFromAmount bool,
) (string, error) {
	client := NewClient()
	service := &Elements{client}
	wallet := readVariableFromPeerswapdConfig("elementsd.rpcwallet")

	params := &SendParams{
		Address:               address,
		Amount:                toBitcoin(amountSats),
		SubtractFeeFromAmount: subtractFeeFromAmount,
	}

	r, err := service.client.call("sendtoaddress", params, "/wallet/"+wallet)
	if err = handleError(err, &r); err != nil {
		log.Printf("Elements rpc: %v", err)
		return "", err
	}

	txid := ""
	// Unmarshal the JSON array into a txid

	err = json.Unmarshal([]byte(r.Result), &txid)
	if err != nil {
		return "", err
	}
	return txid, nil

}
