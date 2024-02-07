package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/elementsproject/peerswap/peerswaprpc"
)

// returns time passed as a srting
func timePassedAgo(t time.Time) string {
	duration := time.Since(t)

	days := int(duration.Hours() / 24)
	hours := int(duration.Hours()) % 24
	minutes := int(duration.Minutes()) % 60
	seconds := int(duration.Seconds()) % 60

	var result string

	if days > 0 {
		result = fmt.Sprintf("%d days ago", days)
	} else if hours > 0 {
		result = fmt.Sprintf("%d hours ago", hours)
	} else if minutes > 0 {
		result = fmt.Sprintf("%d minutes ago", minutes)
	} else {
		result = fmt.Sprintf("%d seconds ago", seconds)
	}

	return result
}

// returns true is the string is present in the array of strings
func stringIsInSlice(whatToFind string, whereToSearch []string) bool {
	for _, s := range whereToSearch {
		if s == whatToFind {
			return true
		}
	}
	return false
}

// formats 100000 as 100,000
func formatWithThousandSeparators(n uint64) string {
	// Convert the integer to a string
	numStr := strconv.FormatUint(n, 10)

	// Determine the length of the number
	length := len(numStr)

	// Calculate the number of separators needed
	separatorCount := (length - 1) / 3

	// Create a new string with separators
	result := make([]byte, length+separatorCount)

	// Iterate through the string in reverse to add separators
	j := 0
	for i := length - 1; i >= 0; i-- {
		result[j] = numStr[i]
		j++
		if i > 0 && (length-i)%3 == 0 {
			result[j] = ','
			j++
		}
	}

	// Reverse the result to get the correct order
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}

	return string(result)
}

var hourGlassRotate = 0

func visualiseSwapStatus(statusText string, rotate bool) string {
	switch statusText {
	case "State_ClaimedCoop":
		return "<a href=\"/\">❌</a>"
	case "State_SwapCanceled":
		return "<a href=\"/\">❌</a>"
	case "State_SendCancel":
		return "<a href=\"/\">❌</a>"
	case "State_ClaimedPreimage":
		return "<a href=\"/\">💰</a>"
	}

	if rotate {
		hourGlassRotate += 1

		if hourGlassRotate == 3 {
			hourGlassRotate = 0
		}

		switch hourGlassRotate {
		case 0:
			return "<a href=\"/\">⏳</a>"
		case 1:
			return "<a href=\"/\">⌛</a>"
		case 2:
			return "<span class=\"rotate-span\"><a href=\"/\">⏳</a></span>" // rotate 90
		}
	}

	return "⌛"
}

// converts a list of peers into an HTML table to display
func convertPeersToHTMLTable(peers []*peerswaprpc.PeerSwapPeer, allowlistedPeers []string, suspiciousPeers []string) string {

	type Table struct {
		AvgLocal uint64
		HtmlBlob string
	}

	var unsortedTable []Table

	for _, peer := range peers {
		var totalLocal uint64
		var totalCapacity uint64

		table := "<table style=\"table-layout:fixed; width: 100%\">"
		table += "<tr style=\"border: 1px dotted\">"
		table += "<td id=\"scramble\" style=\"float: left; text-align: left; width: 80%;\">"

		// alias is a link to open peer details page
		table += "<a href=\"/peer?id=" + peer.NodeId + "\">"

		if stringIsInSlice(peer.NodeId, allowlistedPeers) {
			table += "✅&nbsp"
		} else {
			table += "⛔&nbsp"
		}

		if stringIsInSlice(peer.NodeId, suspiciousPeers) {
			table += "🔍&nbsp"
		}

		table += getNodeAlias(peer.NodeId)
		table += "</a>"
		table += "</td><td style=\"float: right; text-align: right; width:20%;\">"
		table += "<a href=\"/peer?id=" + peer.NodeId + "\">"

		if stringIsInSlice("lbtc", peer.SupportedAssets) {
			table += "🌊&nbsp"
		}
		if stringIsInSlice("btc", peer.SupportedAssets) {
			table += "₿&nbsp"
		}
		if peer.SwapsAllowed {
			table += "✅"
		} else {
			table += "⛔"
		}
		table += "</a>"
		table += "</td></tr></table>"

		table += "<table style=\"table-layout:fixed;\">"

		// Construct channels data
		for _, channel := range peer.Channels {

			// red background for inactive channels
			bc := "#590202"
			if config.ColorScheme == "light" {
				bc = "#fcb6b6"
			}

			if channel.Active {
				// green background for active channels
				bc = "#224725"
				if config.ColorScheme == "light" {
					bc = "#e6ffe8"
				}
			}

			table += "<tr style=\"background-color: " + bc + "\"; >"
			table += "<td id=\"scramble\" style=\"width: 250px; text-align: center\">"
			table += formatWithThousandSeparators(channel.LocalBalance)
			table += "</td><td style=\"width: 25%; text-align: center\">"
			local := channel.LocalBalance
			capacity := channel.LocalBalance + channel.RemoteBalance
			totalLocal += local
			totalCapacity += capacity
			table += "<a href=\"/peer?id=" + peer.NodeId + "\">"
			table += "<progress style=\"width: 100%;\" value=" + strconv.FormatUint(local, 10) + " max=" + strconv.FormatUint(capacity, 10) + ">1</progress>"
			table += "</a></td>"
			table += "<td id=\"scramble\" style=\"width: 250px; text-align: center\">"
			table += formatWithThousandSeparators(channel.RemoteBalance)
			table += "</td></tr>"
		}
		table += "</table>"
		table += "<p style=\"margin:0.5em;\"></p>"

		// count total outbound to sort peers later
		pct := uint64(1000000 * float64(totalLocal) / float64(totalCapacity))

		unsortedTable = append(unsortedTable, Table{
			AvgLocal: pct,
			HtmlBlob: table,
		})
	}

	// sort the table on AvgLocal field
	sort.Slice(unsortedTable, func(i, j int) bool {
		return unsortedTable[i].AvgLocal < unsortedTable[j].AvgLocal
	})

	table := ""
	for _, t := range unsortedTable {
		table += t.HtmlBlob
	}

	return table
}

// converts a list of swaps into an HTML table
func convertSwapsToHTMLTable(swaps []*peerswaprpc.PrettyPrintSwap) string {

	if len(swaps) == 0 {
		return ""
	}

	type Table struct {
		TimeStamp int64
		HtmlBlob  string
	}
	var unsortedTable []Table

	for _, swap := range swaps {
		table := "<tr>"
		table += "<td style=\"width: 30%; text-align: left\">"

		tm := timePassedAgo(time.Unix(swap.CreatedAt, 0).UTC())

		// clicking on timestamp will open swap details page
		table += "<a href=\"/swap?id=" + swap.Id + "\">" + tm + "</a> "
		table += "</td><td style=\"text-align: center\">"
		table += visualiseSwapStatus(swap.State, false) + "&nbsp"
		table += formatWithThousandSeparators(swap.Amount)

		switch swap.Type + swap.Role {
		case "swap-outsender":
			table += " ⚡&nbsp⇨&nbsp"
		case "swap-insender":
			table += " ⚡&nbsp⇦&nbsp"
		case "swap-outreceiver":
			table += " ⚡&nbsp⇦&nbsp"
		case "swap-inreceiver":
			table += " ⚡&nbsp⇨&nbsp"
		}

		switch swap.Asset {
		case "lbtc":
			table += "🌊"
		case "btc":
			table += "₿"
		default:
			table += "?"
		}

		table += "</td><td id=\"scramble\" style=\"overflow-wrap: break-word;\">"

		switch swap.Role {
		case "receiver":
			table += " ⇦&nbsp"
		case "sender":
			table += " ⇨&nbsp"
		default:
			table += " ?&nbsp"
		}

		table += "<a href=\"/peer?id=" + swap.PeerNodeId + "\">"
		table += getNodeAlias(swap.PeerNodeId)
		table += "</a></td></tr>"

		unsortedTable = append(unsortedTable, Table{
			TimeStamp: swap.CreatedAt,
			HtmlBlob:  table,
		})
	}

	// sort the table on TimeStamp field
	sort.Slice(unsortedTable, func(i, j int) bool {
		return unsortedTable[i].TimeStamp > unsortedTable[j].TimeStamp
	})

	var counter uint
	table := "<table style=\"table-layout:fixed; width: 100%\">"
	for _, t := range unsortedTable {
		counter++
		if counter > config.MaxHistory {
			break
		}
		table += t.HtmlBlob
	}
	table += "</table>"
	return table
}

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
				if err := json.Unmarshal([]byte(buf.String()), &nodes); err != nil {
					fmt.Println("Error:", err)
					return id[:20] // shortened id
				}

				if len(nodes.Nodes) > 0 {
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

func findPeerById(peers []*peerswaprpc.PeerSwapPeer, targetId string) *peerswaprpc.PeerSwapPeer {
	for _, p := range peers {
		if p.NodeId == targetId {
			return p
		}
	}
	return nil // Return nil if peer with given ID is not found
}

func getLatestTag() string {

	url := "http://api.github.com/repos/impa10r/peerswap-web/tags"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Println("Error creating request:", err)
		return ""
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Println("Error making request:", err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Println("Failed to fetch tags. Status code:", resp.StatusCode)
		return ""
	}

	var tags []map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&tags)
	if err != nil {
		fmt.Println("Error decoding JSON:", err)
		return ""
	}

	if len(tags) > 0 {
		latestTag := tags[0]["name"].(string)
		return latestTag
	} else {
		log.Println("No tags found in the repository.")
		return ""
	}
}

func setLogging() error {
	// Set log file name
	logFileName := filepath.Join(config.DataDir, "psweb.log")
	var err error
	// Open log file in append mode, create if it doesn't exist
	logFile, err = os.OpenFile(logFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	// Set log output to both file and standard output
	multi := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multi)

	log.SetFlags(log.Ldate | log.Ltime)
	if os.Getenv("DEBUG") == "1" {
		log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	}

	return nil
}

func closeLogFile() {
	if logFile != nil {
		if err := logFile.Close(); err != nil {
			log.Println("Error closing log file:", err)
		}
	}
}

func toSats(amount float64) uint64 {
	return uint64(float64(100000000) * amount)
}
