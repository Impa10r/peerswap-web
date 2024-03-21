package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

func getNodeAlias(id string) string {
	for _, n := range cache {
		if n.PublicKey == id {
			return n.Alias
		}
	}

	url := config.BitcoinApi + "/api/v1/lightning/search?searchText=" + id
	if config.BitcoinApi != "" {
		req, err := http.NewRequest("GET", url, nil)
		if err == nil {
			cl := &http.Client{
				Timeout: 5 * time.Second,
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
					cache = append(cache, AliasCache{
						PublicKey: nodes.Nodes[0].PublicKey,
						Alias:     nodes.Nodes[0].Alias,
					})
					return nodes.Nodes[0].Alias
				} else {
					return id[:20] // shortened id
				}
			}
		}
	}
	return id[:20] // shortened id
}
