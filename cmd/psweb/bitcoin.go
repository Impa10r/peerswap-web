package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"

	"golang.org/x/net/proxy"
)

type Bitcoin struct {
	client *RPCClient
}

// ElementsClient returns an RpcClient
func BitcoinClient() (c *RPCClient) {
	// Connect to Bitcoin Core RPC server
	host := config.BitcoinHost
	user := config.BitcoinUser
	passwd := config.BitcoinPass

	var httpClient *http.Client

	if config.ProxyURL != "" {
		p, err := url.Parse(config.ProxyURL)
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

func getRawTransaction(txid string) (string, error) {
	client := BitcoinClient()
	service := &Bitcoin{client}

	params := []interface{}{txid}

	r, err := service.client.call("getrawtransaction", params, "")
	if err = handleError(err, &r); err != nil {
		log.Printf("getrawtransaction: %v", err)
		return "", err
	}

	raw := ""
	err = json.Unmarshal([]byte(r.Result), &raw)
	if err != nil {
		log.Printf("getrawtransaction unmarshall: %v", err)
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
		log.Printf("gettxoutproof: %v", err)
		return ""
	}

	proof := ""
	err = json.Unmarshal([]byte(r.Result), &proof)
	if err != nil {
		log.Printf("gettxoutproof unmarshall: %v", err)
		return ""
	}
	return proof
}
