package internet

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"

	"peerswap-web/cmd/psweb/config"
)

// fetch node alias from mempool.space
func GetNodeAlias(id string) string {
	if config.Config.BitcoinApi != "" {
		api := config.Config.BitcoinApi + "/api/v1/lightning/search?searchText=" + id
		req, err := http.NewRequest("GET", api, nil)
		if err == nil {
			cl := GetHttpClient(config.Config.BitcoinApi != config.Config.LocalMempool)
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

// fetch high priority fee rate from mempool.space
func GetFeeRate() float64 {
	if config.Config.BitcoinApi != "" {
		api := config.Config.BitcoinApi + "/api/v1/fees/recommended"
		req, err := http.NewRequest("GET", api, nil)
		if err == nil {
			cl := GetHttpClient(config.Config.BitcoinApi != config.Config.LocalMempool)
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
					FastestFee  float64 `json:"fastestFee"`
					HalfHourFee float64 `json:"halfHourFee"`
					HourFee     float64 `json:"hourFee"`
					EconomyFee  float64 `json:"economyFee"`
					MinimumFee  float64 `json:"minimumFee"`
				}

				// Create an instance of the struct to store the parsed data
				var fees Fees

				// Unmarshal the JSON string into the struct
				if err := json.Unmarshal(buf.Bytes(), &fees); err != nil {
					log.Println("Mempool GetFee:", err)
					return 0
				}

				return fees.FastestFee
			} else {
				log.Printf("Mempool GetFee: %v", err)
			}
		}
	}
	return 0
}

// fetch transaction block height from mempool.space
func GetTxHeight(txid string) int32 {
	if config.Config.BitcoinApi != "" {
		api := config.Config.BitcoinApi + "/api/tx/" + txid + "/status"
		req, err := http.NewRequest("GET", api, nil)
		if err == nil {
			cl := GetHttpClient(config.Config.BitcoinApi != config.Config.LocalMempool)
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

// broadcast new transaction to mempool.space
func SendRawTransaction(rawTx string) string {
	if config.Config.BitcoinApi != "" {
		api := config.Config.BitcoinApi + "/api/tx"
		// Define the request body
		requestBody := []byte(rawTx)

		cl := GetHttpClient(config.Config.BitcoinApi != config.Config.LocalMempool)
		if cl == nil {
			return ""
		}

		// Send POST request
		resp, err := cl.Post(api, "application/octet-stream", bytes.NewBuffer(requestBody))
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

// fetch transaction fee from api
func GetBitcoinTxFee(txid string) int64 {
	if config.Config.BitcoinApi != "" {
		api := config.Config.BitcoinApi + "/api/tx/" + txid
		req, err := http.NewRequest("GET", api, nil)
		if err == nil {
			cl := GetHttpClient(true)
			if cl == nil {
				return 0
			}
			resp, err2 := cl.Do(req)
			if err2 == nil {
				defer resp.Body.Close()

				// Create an instance of the struct to store the parsed data
				var tx map[string]interface{}

				err = json.NewDecoder(resp.Body).Decode(&tx)
				if err != nil {
					return 0
				}
				fee := tx["fee"].(float64)
				return int64(fee)
			}
		}
	}
	return 0
}
