package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"text/template"
	"time"

	"github.com/elementsproject/peerswap/peerswaprpc"
	"google.golang.org/grpc"

	"peerswap-web/utils"
)

type Configuration struct {
	RpcHost     string
	ListenPort  string
	ColorScheme string
	MempoolApi  string
	LiquidApi   string
}

type AliasCache struct {
	PublicKey string
	Alias     string
}

var cache []AliasCache
var Config Configuration
var templates = template.New("")

func main() {
	// simulate loading from config file
	Config = Configuration{
		RpcHost:     "localhost:42069",
		ListenPort:  "8088",
		ColorScheme: "dark",                           // dark or light
		MempoolApi:  "https://mempool.space/testnet",  // remove testnet for mainnet
		LiquidApi:   "https://liquid.network/testnet", // remove testnet for mainnet
	}

	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))
	http.HandleFunc("/", homePage)
	http.HandleFunc("/swap", swapDetails)
	http.HandleFunc("/peer", peerDetails)

	// loading and parsing templates preemptively
	var err error
	templates, err = templates.ParseGlob("templates/*.gohtml")
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Listening on http://localhost:" + Config.ListenPort)
	err = http.ListenAndServe(":"+Config.ListenPort, nil)
	if err != nil {
		log.Fatal(err)
	}
}

func homePage(w http.ResponseWriter, r *http.Request) {

	host := Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		log.Fatal(fmt.Errorf("unable to connect to RPC server: %v", err))
	}
	defer cleanup()

	res, err := client.LiquidGetBalance(ctx, &peerswaprpc.GetBalanceRequest{})
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}

	satAmount := res.GetSatAmount()

	res2, err2 := client.ListPeers(ctx, &peerswaprpc.ListPeersRequest{})
	if err2 != nil {
		log.Fatalln(err2)
		http.Error(w, http.StatusText(500), 500)
	}
	peers := res2.GetPeers()

	res3, err3 := client.ListSwaps(ctx, &peerswaprpc.ListSwapsRequest{})
	if err3 != nil {
		log.Fatalln(err3)
		http.Error(w, http.StatusText(500), 500)
	}
	swaps := res3.GetSwaps()

	res4, err4 := client.ReloadPolicyFile(ctx, &peerswaprpc.ReloadPolicyFileRequest{})
	if err4 != nil {
		log.Fatalln(err4)
		http.Error(w, http.StatusText(500), 500)
	}
	allowlistedPeers := res4.GetAllowlistedPeers()

	type Page struct {
		ColorScheme string
		SatAmount   string
		ListPeers   string
		ListSwaps   string
	}

	data := Page{
		ColorScheme: Config.ColorScheme,
		SatAmount:   utils.FormatWithThousandSeparators(satAmount),
		ListPeers:   convertPeersToHTMLTable(peers, allowlistedPeers),
		ListSwaps:   convertSwapsToHTMLTable(swaps),
	}

	// executing template named "homepage"
	err = templates.ExecuteTemplate(w, "homepage", data)
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}
}

func peerDetails(w http.ResponseWriter, r *http.Request) {
	keys, ok := r.URL.Query()["id"]
	if !ok || len(keys[0]) < 1 {
		fmt.Println("URL parameter 'id' is missing")
		http.Error(w, http.StatusText(500), 500)
		return
	}

	id := keys[0]
	host := Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		log.Fatal(fmt.Errorf("unable to connect to RPC server: %v", err))
	}
	defer cleanup()

	res, err := client.ListPeers(ctx, &peerswaprpc.ListPeersRequest{})
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}
	peers := res.GetPeers()
	peer := findPeerById(peers, id)

	if peer == nil {
		log.Fatalln("Peer not found")
		http.Error(w, http.StatusText(500), 500)
	}

	type Page struct {
		ColorScheme string
		Peer        *peerswaprpc.PeerSwapPeer
		PeerAlias   string
	}

	data := Page{
		ColorScheme: Config.ColorScheme,
		Peer:        peer,
		PeerAlias:   getNodeAlias(peer.NodeId),
	}

	// executing template named "peer"
	err = templates.ExecuteTemplate(w, "peer", data)
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}
}

func swapDetails(w http.ResponseWriter, r *http.Request) {
	keys, ok := r.URL.Query()["id"]
	if !ok || len(keys[0]) < 1 {
		fmt.Println("URL parameter 'id' is missing")
		http.Error(w, http.StatusText(500), 500)
		return
	}

	id := keys[0]
	host := Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		log.Fatal(fmt.Errorf("unable to connect to RPC server: %v", err))
	}
	defer cleanup()

	res, err := client.GetSwap(ctx, &peerswaprpc.GetSwapRequest{
		SwapId: id,
	})
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}

	swap := res.GetSwap()

	type Page struct {
		ColorScheme    string
		Swap           *peerswaprpc.PrettyPrintSwap
		CreatedAt      string
		TxUrl          string // to open tx on mempool.space or liquid.network
		NodeUrl        string // to open tx on mempool.space
		InitiatorAlias string
		PeerAlias      string
	}

	url := Config.MempoolApi
	if swap.Asset == "lbtc" {
		url = Config.LiquidApi
	}

	data := Page{
		ColorScheme:    Config.ColorScheme,
		Swap:           swap,
		CreatedAt:      time.Unix(swap.CreatedAt, 0).UTC().Format("2006-01-02 15:04:05"),
		TxUrl:          url + "/tx/",
		NodeUrl:        Config.MempoolApi + "/lightning/node/",
		InitiatorAlias: getNodeAlias(swap.InitiatorNodeId),
		PeerAlias:      getNodeAlias(swap.PeerNodeId),
	}

	// executing template named "swap"
	err = templates.ExecuteTemplate(w, "swap", data)
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}
}

func getClient(rpcServer string) (peerswaprpc.PeerSwapClient, func(), error) {
	conn, err := getClientConn(rpcServer)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { conn.Close() }

	psClient := peerswaprpc.NewPeerSwapClient(conn)
	return psClient, cleanup, nil
}

func getClientConn(address string) (*grpc.ClientConn, error) {

	maxMsgRecvSize := grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200)
	opts := []grpc.DialOption{
		grpc.WithDefaultCallOptions(maxMsgRecvSize),
		grpc.WithInsecure(),
	}

	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	return conn, nil
}

// converts a list of peers into an HTML table to display
func convertPeersToHTMLTable(peers []*peerswaprpc.PeerSwapPeer, allowlistedPeers []string) string {

	type Table struct {
		AvgLocal int
		HtmlBlob string
	}

	var unsortedTable []Table

	for _, peer := range peers {
		var totalLocal uint64
		var totalCapacity uint64

		table := "<table style=\"table-layout:fixed; width: 100%\">"
		table += "<tr><td style=\"float: left; text-align: left; width: 80%;\">"
		if utils.StringIsInSlice(peer.NodeId, allowlistedPeers) {
			table += "✅&nbsp&nbsp"
		} else {
			table += "⛔&nbsp&nbsp"
		}
		// alias is a link to open peer details page
		table += "<a href=\"/peer?id=" + peer.NodeId + "\">"
		table += getNodeAlias(peer.NodeId) + "</a>"
		table += "</td><td style=\"float: right; text-align: right; width:20%;\">"
		if peer.SwapsAllowed {
			table += "✅&nbsp"
		} else {
			table += "⛔&nbsp"
		}
		if utils.StringIsInSlice("lbtc", peer.SupportedAssets) {
			table += "🌊"
		}
		if utils.StringIsInSlice("btc", peer.SupportedAssets) {
			table += "₿"
		}
		table += "</div></td></tr></table>"

		table += "<table style=\"table-layout:fixed;\">"

		// Construct channels data
		for _, channel := range peer.Channels {

			// red background for inactive channels
			bc := "#590202"
			if Config.ColorScheme == "light" {
				bc = "#fcb6b6"
			}

			if channel.Active {
				// green background for active channels
				bc = "#224725"
				if Config.ColorScheme == "light" {
					bc = "#e6ffe8"
				}
			}

			table += "<tr style=\"background-color: " + bc + "\"; >"
			table += "<td style=\"width: 250px; text-align: center\">"
			table += utils.FormatWithThousandSeparators(channel.LocalBalance)
			table += "</td><td>"
			local := channel.LocalBalance
			capacity := channel.LocalBalance + channel.RemoteBalance
			totalLocal += local
			totalCapacity += capacity
			table += "<progress value=" + strconv.FormatUint(local, 10) + " max=" + strconv.FormatUint(capacity, 10) + "></progress>"
			table += "</td><td>"
			table += "<td style=\"width: 250px; text-align: center\">"
			table += utils.FormatWithThousandSeparators(channel.RemoteBalance)
			table += "</td></tr>"
		}
		table += "</table>"

		pct := int(float64(totalLocal) / float64(totalCapacity) * 100)

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

	table := "<table style=\"table-layout:fixed;\">"

	for _, swap := range swaps {
		table += "<tr><td style=\"width: 30%; text-align: left\">"

		tm := utils.TimePassedAgo(time.Unix(swap.CreatedAt, 0).UTC())

		// clicking on timestamp will open swap details page
		table += "<a href=\"/swap?id=" + swap.Id + "\">" + tm + "</a> "
		table += "</td><td>"

		switch swap.State {
		case "State_SwapCanceled":
			table += "❌&nbsp"
		case "State_ClaimedPreimage":
			table += "💰&nbsp"
		default:
			table += "?&nbsp"
		}

		table += utils.FormatWithThousandSeparators(swap.Amount)

		switch swap.Type {
		case "swap-out":
			table += " ⚡&nbsp⇨&nbsp"
		case "swap-in":
			table += " ⚡&nbsp⇦&nbsp"
		default:
			table += " !!swap type error!!"
		}

		switch swap.Asset {
		case "lbtc":
			table += "🌊"
		case "btc":
			table += "₿"
		default:
			table += "!!swap asset error!!"
		}

		table += "</td><td>"

		switch swap.Role {
		case "receiver":
			table += " ⇩&nbsp"
		case "sender":
			table += " ⇧&nbsp"
		default:
			table += " ?&nbsp"
		}

		table += getNodeAlias(swap.PeerNodeId)
		table += "</td></tr>"
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

	url := Config.MempoolApi + "/api/v1/lightning/search?searchText=" + id
	if Config.MempoolApi != "" {
		req, err := http.NewRequest("GET", url, nil)
		if err == nil {
			cl := &http.Client{}
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
