package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

func getHttpClient() *http.Client {
	var httpClient *http.Client

	if config.ProxyURL != "" {
		p, err := url.Parse(config.ProxyURL)
		if err != nil {
			log.Println("mempool getHttpClient:", err)
			return nil
		}
		dialer, err := proxy.SOCKS5("tcp", p.Host, nil, proxy.Direct)
		if err != nil {
			log.Println("mempool getHttpClient:", err)
			return nil
		}
		httpClient = &http.Client{
			Transport: &http.Transport{
				Dial: dialer.Dial,
			},
			Timeout: 5 * time.Second,
		}
	} else {
		httpClient = &http.Client{
			Timeout: 5 * time.Second,
		}
	}
	return httpClient
}

func mempoolGetNodeAlias(id string) string {
	if config.BitcoinApi != "" {
		api := config.BitcoinApi + "/api/v1/lightning/search?searchText=" + id
		req, err := http.NewRequest("GET", api, nil)
		if err == nil {
			cl := getHttpClient()
			if cl == nil {
				return id[:20] // shortened id
			}
			resp, err2 := cl.Do(req)
			if err2 == nil {
				defer resp.Body.Close()
				buf := new(bytes.Buffer)
				_, _ = buf.ReadFrom(resp.Body)

				// Define a struct to match the JSON structure
				type Node struct {
					PublicKey string `json:"public_key"`
					Alias     string `json:"alias"`
					Capacity  uint64 `json:"capacity"`
					Channels  uint   `json:"channels"`
					Status    uint   `json:"status"`
				}
				type Nodes struct {
					Nodes    []Node   `json:"nodes"`
					Channels []string `json:"channels"`
				}

				// Create an instance of the struct to store the parsed data
				var nodes Nodes

				// Unmarshal the JSON string into the struct
				if err := json.Unmarshal(buf.Bytes(), &nodes); err != nil {
					log.Println("Mempool Error:", err)
					return id[:20] // shortened id
				}

				if len(nodes.Nodes) > 0 && len(nodes.Nodes[0].Alias) > 0 {
					return nodes.Nodes[0].Alias
				} else {
					return id[:20] // shortened id
				}
			}
		}
	}
	return id[:20] // shortened id
}

func mempoolGetFee() uint32 {
	if config.BitcoinApi != "" {
		api := config.BitcoinApi + "/api/v1/fees/recommended"
		req, err := http.NewRequest("GET", api, nil)
		if err == nil {
			cl := getHttpClient()
			if cl == nil {
				return 0
			}
			resp, err2 := cl.Do(req)
			if err2 == nil {
				defer resp.Body.Close()
				buf := new(bytes.Buffer)
				_, _ = buf.ReadFrom(resp.Body)

				// Define a struct to match the JSON structure
				type Fees struct {
					FastestFee  uint32 `json:"fastestFee"`
					HalfHourFee uint32 `json:"halfHourFee"`
					HourFee     uint32 `json:"hourFee"`
					EconomyFee  uint32 `json:"economyFee"`
					MinimumFee  uint32 `json:"minimumFee"`
				}

				// Create an instance of the struct to store the parsed data
				var fees Fees

				// Unmarshal the JSON string into the struct
				if err := json.Unmarshal(buf.Bytes(), &fees); err != nil {
					log.Println("Mempool Error:", err)
					return 0
				}

				return fees.FastestFee
			}
		}
	}
	return 0
}
