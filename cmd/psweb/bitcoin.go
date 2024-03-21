package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
)

type Bitcoin struct {
	client *RPCClient
}

// ElementsClient returns an RpcClient
func BitcoinClient() (c *RPCClient) {
	// Connect to Bitcoin Core RPC server
	host := config.BitcoinHost
	user := config.BitcoinUser
	passwd := config.BitcoinPasswd
	port := config.BitcoinPort

	httpClient := &http.Client{}
	serverAddr := fmt.Sprintf("http://%s:%s", host, port)
	c = &RPCClient{serverAddr: serverAddr, user: user, passwd: passwd, httpClient: httpClient, timeout: 5}
	return
}

func getRawTransaction(txid string) (string, error) {
	client := BitcoinClient()
	service := &Bitcoin{client}

	params := []interface{}{txid}

	r, err := service.client.call("getrawtransaction", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("Bitcoin rpc: %v", err)
		return "", err
	}

	raw := ""
	err = json.Unmarshal([]byte(r.Result), &raw)
	if err != nil {
		log.Printf("Bitcoin rpc: %v", err)
		return "", err
	}
	return raw, nil
}

func getTxOutProof(txid string) string {
	client := BitcoinClient()
	service := &Bitcoin{client}

	params := []interface{}{[]string{txid}}

	r, err := service.client.call("gettxoutproof", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("Bitcoin rpc: %v", err)
		return ""
	}

	proof := ""
	err = json.Unmarshal([]byte(r.Result), &proof)
	if err != nil {
		log.Printf("Bitcoin rpc: %v", err)
		return ""
	}
	return proof
}