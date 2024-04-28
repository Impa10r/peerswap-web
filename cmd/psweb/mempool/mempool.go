package mempool

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"

	"peerswap-web/cmd/psweb/config"

	"golang.org/x/net/proxy"
)

func getHttpClient() *http.Client {
	var httpClient *http.Client

	if config.Config.ProxyURL != "" {
		p, err := url.Parse(config.Config.ProxyURL)
		if err != nil {
			log.Println("Mempool getHttpClient:", err)
			return nil
		}
		dialer, err := proxy.SOCKS5("tcp", p.Host, nil, proxy.Direct)
		if err != nil {
			log.Println("Mempool getHttpClient:", err)
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

func GetNodeAlias(id string) string {
	if config.Config.BitcoinApi != "" {
		api := config.Config.BitcoinApi + "/api/v1/lightning/search?searchText=" + id
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
					log.Println("Mempool GetNodeAlias:", err)
					return id[:20] // shortened id
				}

				if len(nodes.Nodes) > 0 && len(nodes.Nodes[0].Alias) > 0 {
					return nodes.Nodes[0].Alias
				} else {
					return ""
				}
			}
		}
	}
	return ""
}

func GetFee() uint32 {
	if config.Config.BitcoinApi != "" {
		api := config.Config.BitcoinApi + "/api/v1/fees/recommended"
		req, err := http.NewRequest("GET", api, nil)
		if err == nil {
			cl := getHttpClient()
			if cl == nil {
				return 0
			}
			resp, err2 := cl.Do(req)
			if err2 == nil {
				defer resp.Body.Close()
				if resp.StatusCode != 200 {
					return 0
				}

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
					log.Println("Mempool GetFee:", err)
					return 0
				}

				return fees.FastestFee
			}
		}
	}
	return 0
}

func GetTxHeight(txid string) int32 {
	if config.Config.BitcoinApi != "" {
		api := config.Config.BitcoinApi + "/api/tx/" + txid + "/status"
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
				type Status struct {
					Confirmed   bool   `json:"confirmed"`
					BlockHeight int32  `json:"block_height"`
					BlockHash   string `json:"block_hash"`
					BlockTime   uint64 `json:"block_time"`
				}

				// Create an instance of the struct to store the parsed data
				var status Status

				// Unmarshal the JSON string into the struct
				if err := json.Unmarshal(buf.Bytes(), &status); err != nil {
					log.Println("Mempool GetTxHeight:", err)
					return 0
				}

				return status.BlockHeight
			}
		}
	}
	return 0
}

func SendRawTransaction(rawTx string) string {
	if config.Config.BitcoinApi != "" {
		api := config.Config.BitcoinApi + "/api/tx"
		// Define the request body
		requestBody := []byte(rawTx)

		// Send POST request
		resp, err := http.Post(api, "application/octet-stream", bytes.NewBuffer(requestBody))
		if err != nil {
			log.Println("Mempool SendRawTransaction:", err)
			return ""
		}
		defer resp.Body.Close()

		// Read the response body
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Println("Mempool SendRawTransaction:", err)
			return ""
		}

		// Return the response
		return string(responseBody)
	}
	return ""
}
