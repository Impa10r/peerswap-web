package bitcoin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"peerswap-web/cmd/psweb/config"

	"golang.org/x/net/proxy"
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

type Bitcoin struct {
	client *RPCClient
}

// BitcoinClient returns an RpcClient
func BitcoinClient() (c *RPCClient) {
	// Connect to Bitcoin Core RPC server
	host := config.Config.BitcoinHost
	user := config.Config.BitcoinUser
	passwd := config.Config.BitcoinPass

	var httpClient *http.Client

	if config.Config.ProxyURL != "" {
		p, err := url.Parse(config.Config.ProxyURL)
		if err != nil {
			return nil
		}
		dialer, err := proxy.SOCKS5("tcp", p.Host, nil, proxy.Direct)
		if err != nil {
			return nil
		}
		httpClient = &http.Client{
			Transport: &http.Transport{
				Dial: dialer.Dial,
			},
		}
	} else {
		httpClient = &http.Client{}
	}

	serverAddr := host
	c = &RPCClient{
		serverAddr: serverAddr,
		user:       user,
		passwd:     passwd,
		httpClient: httpClient,
		timeout:    30,
	}
	return
}

func GetRawTransaction(txid string) (string, error) {
	client := BitcoinClient()
	service := &Bitcoin{client}

	params := []interface{}{txid}

	r, err := service.client.call("getrawtransaction", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("GetRawTransaction: %v", err)
		return "", err
	}

	raw := ""
	err = json.Unmarshal([]byte(r.Result), &raw)
	if err != nil {
		log.Printf("GetRawTransaction unmarshall: %v", err)
		return "", err
	}
	return raw, nil
}

func GetTxOutProof(txid string) (string, error) {
	client := BitcoinClient()
	service := &Bitcoin{client}

	params := []interface{}{[]string{txid}}

	r, err := service.client.call("gettxoutproof", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("GetTxOutProof: %v", err)
		return "", err
	}

	proof := ""
	err = json.Unmarshal([]byte(r.Result), &proof)
	if err != nil {
		log.Printf("GetTxOutProof unmarshall: %v", err)
		return "", err
	}
	return proof, nil
}

type Transaction struct {
	TXID     string `json:"txid"`
	Hash     string `json:"hash"`
	Version  int    `json:"version"`
	Size     int    `json:"size"`
	VSize    int    `json:"vsize"`
	Weight   int    `json:"weight"`
	Locktime int    `json:"locktime"`
	Vin      []Vin  `json:"vin"`
	Vout     []Vout `json:"vout"`
}

type Vin struct {
	TXID      string    `json:"txid"`
	Vout      uint      `json:"vout"`
	ScriptSig ScriptSig `json:"scriptSig"`
	Sequence  uint32    `json:"sequence"`
}

type ScriptSig struct {
	Asm string `json:"asm"`
	Hex string `json:"hex"`
}

type Vout struct {
	Value        float64      `json:"value"`
	N            uint         `json:"n"`
	ScriptPubKey ScriptPubKey `json:"scriptPubKey"`
}

type ScriptPubKey struct {
	Asm     string `json:"asm"`
	Desc    string `json:"desc"`
	Hex     string `json:"hex"`
	Address string `json:"address"`
	Type    string `json:"type"`
}

func DecodeRawTransaction(hexstring string) (*Transaction, error) {
	client := BitcoinClient()
	service := &Bitcoin{client}

	params := []string{hexstring}

	r, err := service.client.call("decoderawtransaction", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("DecodeRawTransaction: %v", err)
		return nil, err
	}

	var transaction Transaction
	err = json.Unmarshal([]byte(r.Result), &transaction)
	if err != nil {
		log.Printf("DecodeRawTransaction unmarshall: %v", err)
		return nil, err
	}

	return &transaction, nil
}

func SendRawTransaction(hexstring string) (string, error) {
	client := BitcoinClient()
	service := &Bitcoin{client}

	params := []string{hexstring}

	r, err := service.client.call("sendrawtransaction", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("SendRawTransaction: %v", err)
		return "", err
	}

	txid := ""
	err = json.Unmarshal([]byte(r.Result), &txid)
	if err != nil {
		log.Printf("SendRawTransaction unmarshall: %v", err)
		return "", err
	}

	return txid, nil
}
