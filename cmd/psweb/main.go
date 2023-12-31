package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"text/template"
	"time"

	"github.com/elementsproject/peerswap/peerswaprpc"
	"google.golang.org/grpc"

	"peerswap-web/utils"
)

type AliasCache struct {
	PublicKey string
	Alias     string
}

var (
	cache     []AliasCache
	Config    utils.Configuration
	templates = template.New("")
	//go:embed templates/*.gohtml
	tplFolder embed.FS // embeds the templates folder into variable tplFolder
)

const (
	version = "1.0.0"
)

func main() {
	var (
		configFile  = flag.String("configfile", "", "Path/filename to store config JSON")
		showHelp    = flag.Bool("help", false, "Show help")
		showVersion = flag.Bool("version", false, "Show version")
	)

	flag.Parse()

	if *showHelp {
		showHelpMessage()
		return
	}

	if *showVersion {
		showVersionInfo()
		return
	}

	// loading from the config file or assigning defaults
	utils.LoadConfig(*configFile, &Config)

	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))
	http.HandleFunc("/", homePage)
	http.HandleFunc("/swap", onSwap)
	http.HandleFunc("/peer", onPeer)
	http.HandleFunc("/submit", onSubmit)
	http.HandleFunc("/config", onConfig)

	// Get all HTML template files from the embedded filesystem
	templateFiles, err := tplFolder.ReadDir("templates")
	if err != nil {
		panic(err)
	}

	// Store template names
	var templateNames []string
	for _, file := range templateFiles {
		if !file.IsDir() && filepath.Ext(file.Name()) == ".gohtml" {
			templateNames = append(templateNames, "templates/"+file.Name())
		}
	}

	// Parse all template files in the templates directory
	templates = template.Must(templates.ParseFS(tplFolder, templateNames...))

	if *configFile != "" {
		// wait a little in case it was an autorestart
		time.Sleep(2 * time.Second)
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
		log.Println(fmt.Errorf("unable to connect to RPC server: %v", err))
		// display the error to the web page
		redirectWithError(w, r, "/config?", err)
		return
	}
	defer cleanup()

	res, err := client.LiquidGetBalance(ctx, &peerswaprpc.GetBalanceRequest{})
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}

	satAmount := res.GetSatAmount()

	res2, err := client.ListPeers(ctx, &peerswaprpc.ListPeersRequest{})
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}
	peers := res2.GetPeers()

	res3, err := client.ListSwaps(ctx, &peerswaprpc.ListSwapsRequest{})
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}
	swaps := res3.GetSwaps()

	res4, err := client.ReloadPolicyFile(ctx, &peerswaprpc.ReloadPolicyFileRequest{})
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}
	allowlistedPeers := res4.GetAllowlistedPeers()

	//check for error message to display
	message := ""
	keys, ok := r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		message = keys[0]
	}

	type Page struct {
		Message     string
		ColorScheme string
		SatAmount   string
		ListPeers   string
		ListSwaps   string
	}

	data := Page{
		Message:     message,
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

func onPeer(w http.ResponseWriter, r *http.Request) {
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
		log.Printf("unable to connect to RPC server: %v", err)
		redirectWithError(w, r, "/peer?id="+id+"&", err)
		return
	}
	defer cleanup()

	res, err := client.ListPeers(ctx, &peerswaprpc.ListPeersRequest{})
	if err != nil {
		log.Printf("unable to connect to RPC server: %v", err)
		redirectWithError(w, r, "/peer?id="+id+"&", err)
		return
	}
	peers := res.GetPeers()
	peer := findPeerById(peers, id)

	if peer == nil {
		log.Printf("unable to connect to RPC server: %v", err)
		redirectWithError(w, r, "/peer?id="+id+"&", err)
		return
	}

	res2, err := client.ReloadPolicyFile(ctx, &peerswaprpc.ReloadPolicyFileRequest{})
	if err != nil {
		log.Printf("unable to connect to RPC server: %v", err)
		redirectWithError(w, r, "/peer?id="+id+"&", err)
		return
	}
	allowlistedPeers := res2.GetAllowlistedPeers()

	res3, err := client.LiquidGetBalance(ctx, &peerswaprpc.GetBalanceRequest{})
	if err != nil {
		log.Printf("unable to connect to RPC server: %v", err)
		redirectWithError(w, r, "/peer?id="+id+"&", err)
		return
	}

	satAmount := res3.GetSatAmount()

	//check for error message to display
	message := ""
	keys, ok = r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		message = keys[0]
	}

	type Page struct {
		Message     string
		ColorScheme string
		Peer        *peerswaprpc.PeerSwapPeer
		PeerAlias   string
		NodeUrl     string // to open tx on mempool.space
		Allowed     bool
		LBTC        bool
		BTC         bool
		SatAmount   string
	}

	data := Page{
		Message:     message,
		ColorScheme: Config.ColorScheme,
		Peer:        peer,
		PeerAlias:   getNodeAlias(peer.NodeId),
		NodeUrl:     Config.MempoolApi + "/lightning/node/",
		Allowed:     utils.StringIsInSlice(peer.NodeId, allowlistedPeers),
		BTC:         utils.StringIsInSlice("btc", peer.SupportedAssets),
		LBTC:        utils.StringIsInSlice("lbtc", peer.SupportedAssets),
		SatAmount:   utils.FormatWithThousandSeparators(satAmount),
	}

	// executing template named "peer"
	err = templates.ExecuteTemplate(w, "peer", data)
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}
}

func onSwap(w http.ResponseWriter, r *http.Request) {
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
		log.Printf("unable to connect to RPC server: %v", err)
		redirectWithError(w, r, "/swap?id="+id+"&", err)
		return
	}
	defer cleanup()

	res, err := client.GetSwap(ctx, &peerswaprpc.GetSwapRequest{
		SwapId: id,
	})
	if err != nil {
		log.Printf("onSwap: %v", err)
		redirectWithError(w, r, "/swap?id="+id+"&", err)
		return
	}

	swap := res.GetSwap()

	//check for error message to display
	message := ""
	keys, ok = r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		message = keys[0]
	}

	type Page struct {
		Message        string
		ColorScheme    string
		Swap           *peerswaprpc.PrettyPrintSwap
		CreatedAt      string
		TxUrl          string // to open tx on mempool.space or liquid.network
		NodeUrl        string // to open a node on mempool.space
		InitiatorAlias string
		PeerAlias      string
		SwapStatusChar string
	}

	url := Config.MempoolApi
	if swap.Asset == "lbtc" {
		url = Config.LiquidApi
	}

	data := Page{
		Message:        message,
		ColorScheme:    Config.ColorScheme,
		Swap:           swap,
		CreatedAt:      time.Unix(swap.CreatedAt, 0).UTC().Format("2006-01-02 15:04:05"),
		TxUrl:          url + "/tx/",
		NodeUrl:        Config.MempoolApi + "/lightning/node/",
		InitiatorAlias: getNodeAlias(swap.InitiatorNodeId),
		PeerAlias:      getNodeAlias(swap.PeerNodeId),
		SwapStatusChar: utils.VisualiseSwapStatus(swap.State),
	}

	// executing template named "swap"
	err = templates.ExecuteTemplate(w, "swap", data)
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}
}

func onConfig(w http.ResponseWriter, r *http.Request) {
	//check for error message to display
	message := ""
	keys, ok := r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		message = keys[0]
	}

	type Page struct {
		Message     string
		ColorScheme string
		Config      utils.Configuration
	}

	data := Page{
		Message:     message,
		ColorScheme: Config.ColorScheme,
		Config:      Config,
	}

	// executing template named "error"
	err := templates.ExecuteTemplate(w, "config", data)
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
		AvgLocal uint64
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

		if utils.StringIsInSlice("lbtc", peer.SupportedAssets) {
			table += "🌊&nbsp"
		}
		if utils.StringIsInSlice("btc", peer.SupportedAssets) {
			table += "₿&nbsp"
		}
		if peer.SwapsAllowed {
			table += "✅"
		} else {
			table += "⛔"
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
			table += "<a href=\"/peer?id=" + peer.NodeId + "\">"
			table += "<progress value=" + strconv.FormatUint(local, 10) + " max=" + strconv.FormatUint(capacity, 10) + "> </progress>"
			table += "</a></td><td>"
			table += "<td style=\"width: 250px; text-align: center\">"
			table += utils.FormatWithThousandSeparators(channel.RemoteBalance)
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

	type Table struct {
		TimeStamp int64
		HtmlBlob  string
	}
	var unsortedTable []Table

	for _, swap := range swaps {
		table := "<table style=\"table-layout:fixed; width: 100%\">"
		table += "<tr><td style=\"width: 30%; text-align: left\">"

		tm := utils.TimePassedAgo(time.Unix(swap.CreatedAt, 0).UTC())

		// clicking on timestamp will open swap details page
		table += "<a href=\"/swap?id=" + swap.Id + "\">" + tm + "</a> "
		table += "</td><td style=\"text-align: center\">"
		table += utils.VisualiseSwapStatus(swap.State) + "&nbsp"
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

		unsortedTable = append(unsortedTable, Table{
			TimeStamp: swap.CreatedAt,
			HtmlBlob:  table,
		})
	}

	// sort the table on TimeStamp field
	sort.Slice(unsortedTable, func(i, j int) bool {
		return unsortedTable[i].TimeStamp > unsortedTable[j].TimeStamp
	})

	table := ""
	for _, t := range unsortedTable {
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

func onSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// Parse the form data
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Error parsing form data", http.StatusBadRequest)
			return
		}

		action := r.FormValue("action")

		if action == "saveConfig" {

			mustRestart := Config.ListenPort != r.FormValue("listenPort")

			Config.RpcHost = r.FormValue("rpcHost")
			Config.ListenPort = r.FormValue("listenPort")
			Config.ColorScheme = r.FormValue("colorScheme")
			Config.MempoolApi = r.FormValue("mempoolApi")
			Config.LiquidApi = r.FormValue("liquidApi")
			Config.ConfigFile = r.FormValue("configFile")

			err := utils.SaveConfig(&Config)
			if err != nil {
				redirectWithError(w, r, "/config?", err)
				return
			}

			if mustRestart {
				// Execute a new instance of the program passing --configfile as a parameter
				cmd := exec.Command(os.Args[0], "--configfile="+Config.ConfigFile)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr

				if err := cmd.Start(); err != nil {
					fmt.Println("Failed to restart:", err)
					return
				}

				fmt.Println("Restarted successfully!")
				os.Exit(0)
			}
			if err != nil {
				redirectWithError(w, r, "/?", err)
			} else {
				http.Redirect(w, r, "/", http.StatusSeeOther)
			}
			return
		}

		nodeId := r.FormValue("nodeId")

		host := Config.RpcHost
		ctx := context.Background()

		client, cleanup, err := getClient(host)
		if err != nil {
			redirectWithError(w, r, "/config?", err)
			return
		}
		defer cleanup()

		switch action {
		case "addPeer":
			_, err := client.AddPeer(ctx, &peerswaprpc.AddPeerRequest{
				PeerPubkey: nodeId,
			})
			if err != nil {
				redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
				return
			}
			// Redirect to peer page
			http.Redirect(w, r, "/peer?id="+nodeId, http.StatusSeeOther)
			return

		case "removePeer":
			_, err := client.RemovePeer(ctx, &peerswaprpc.RemovePeerRequest{
				PeerPubkey: nodeId,
			})
			if err != nil {
				redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
				return
			}
			// Redirect to peer page
			http.Redirect(w, r, "/peer?id="+nodeId, http.StatusSeeOther)
			return

		case "doSwap":
			swapAmount, err := strconv.ParseUint(r.FormValue("swapAmount"), 10, 64)
			if err != nil {
				redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
				return
			}

			channelId, err := strconv.ParseUint(r.FormValue("channelId"), 10, 64)
			if err != nil {
				redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
				return
			}

			switch r.FormValue("direction") {
			case "swapIn":
				resp, err := client.SwapIn(ctx, &peerswaprpc.SwapInRequest{
					SwapAmount: swapAmount,
					ChannelId:  channelId,
					Asset:      r.FormValue("asset"),
					Force:      r.FormValue("force") == "true",
				})
				if err != nil {
					redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
					return
				}
				// Redirect to swap page to follow the swap
				http.Redirect(w, r, "/swap?id="+resp.Swap.Id, http.StatusSeeOther)

			case "swapOut":
				resp, err := client.SwapOut(ctx, &peerswaprpc.SwapOutRequest{
					SwapAmount: swapAmount,
					ChannelId:  channelId,
					Asset:      r.FormValue("asset"),
					Force:      r.FormValue("force") == "true",
				})
				if err != nil {
					redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
					return
				}
				// Redirect to swap page to follow the swap
				http.Redirect(w, r, "/swap?id="+resp.Swap.Id, http.StatusSeeOther)
			}

		default:
			// Redirect to index page on any other input
			log.Println("unknonw action: ", action)
			http.Redirect(w, r, "/", http.StatusSeeOther)
		}
	} else {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func redirectWithError(w http.ResponseWriter, r *http.Request, redirectUrl string, err error) {
	// display the error to the web page header
	msg := url.QueryEscape(fmt.Sprintln(err))
	http.Redirect(w, r, redirectUrl+"err="+msg, http.StatusSeeOther)
}

func showHelpMessage() {
	fmt.Println("A lightweight server-side rendered Web UI for PeerSwap LND, which allows trustless P2P submarine swaps Lightning <-> BTC and Lightning <-> L-BTC.")
	fmt.Println("Usage:")
	flag.PrintDefaults()
}

func showVersionInfo() {
	fmt.Println("Version:", version)
}
