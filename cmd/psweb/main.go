package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
	"text/template"
	"time"

	"github.com/elementsproject/peerswap/peerswaprpc"
	"github.com/gorilla/mux"
	"github.com/lightningnetwork/lnd/lnrpc"
)

type AliasCache struct {
	PublicKey string
	Alias     string
}

var (
	cache     []AliasCache
	templates = template.New("")
	//go:embed static
	staticFiles embed.FS
	//go:embed templates/*.gohtml
	tplFolder embed.FS
	logFile   *os.File
)

const version = "v1.2.1"

func main() {

	var (
		dataDir     = flag.String("datadir", "", "Path to config folder")
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
	loadConfig(*dataDir)

	// set logging params
	err := setLogging()
	if err != nil {
		log.Fatal(err)
	}

	// on first start without config there will be no elements user and password
	if config.ElementsPass == "" || config.ElementsUser == "" {
		// check in peerswap.conf
		config.ElementsPass = getPeerswapConfSetting("elementsd.rpcpass")
		config.ElementsUser = getPeerswapConfSetting("elementsd.rpcuser")

		// check if they were passed as env
		if config.ElementsUser == "" && os.Getenv("ELEMENTS_USER") != "" {
			config.ElementsUser = os.Getenv("ELEMENTS_USER")
		}
		if config.ElementsPass == "" && os.Getenv("ELEMENTS_PASS") != "" {
			config.ElementsPass = os.Getenv("ELEMENTS_PASS")
		}

		saveConfig()
		// if ElementPass is still empty, this will create temporary peerswap.conf with Liquid disabled
		savePeerSwapdConfig()
	}

	// Get all HTML template files from the embedded filesystem
	templateFiles, err := tplFolder.ReadDir("templates")
	if err != nil {
		log.Fatal(err)
	}

	// Store template names
	var templateNames []string
	for _, file := range templateFiles {
		if !file.IsDir() && filepath.Ext(file.Name()) == ".gohtml" {
			templateNames = append(templateNames, filepath.Join("templates", file.Name()))
		}
	}

	// Parse all template files in the templates directory
	templates = template.Must(templates.
		Funcs(template.FuncMap{
			"sats": toSats,
			"u":    toUint,
			"fmt":  formatWithThousandSeparators,
			"m":    toMil,
		}).
		ParseFS(tplFolder, templateNames...))

	// create an embedded Filesystem
	var staticFS = http.FS(staticFiles)
	fs := http.FileServer(staticFS)

	// Serve static files
	http.Handle("/static/", fs)

	r := mux.NewRouter()

	// Serve templates
	r.HandleFunc("/", indexHandler)
	r.HandleFunc("/swap", swapHandler)
	r.HandleFunc("/peer", peerHandler)
	r.HandleFunc("/submit", submitHandler)
	r.HandleFunc("/save", saveConfigHandler)
	r.HandleFunc("/config", configHandler)
	r.HandleFunc("/stop", stopHandler)
	r.HandleFunc("/update", updateHandler)
	r.HandleFunc("/liquid", liquidHandler)
	r.HandleFunc("/loading", loadingHandler)
	r.HandleFunc("/log", logHandler)
	r.HandleFunc("/logapi", logApiHandler)
	r.HandleFunc("/backup", backupHandler)
	r.HandleFunc("/bitcoin", bitcoinHandler)
	r.HandleFunc("/pegin", peginHandler)

	// Start the server
	http.Handle("/", r)
	go func() {
		if err := http.ListenAndServe(":"+config.ListenPort, nil); err != nil {
			log.Fatal(err)
		}
	}()

	log.Println("Listening on http://localhost:" + config.ListenPort)

	// Run every minute
	go startTimer()

	// Start Telegram bot
	go telegramStart()

	// Handle termination signals
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for termination signal
	<-signalChan
	log.Println("Received termination signal")

	// close log
	closeLogFile()

	// Exit the program gracefully
	os.Exit(0)
}

func indexHandler(w http.ResponseWriter, r *http.Request) {

	if config.ElementsPass == "" || config.ElementsUser == "" {
		http.Redirect(w, r, "/config?err=welcome", http.StatusSeeOther)
		return
	}

	host := config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		log.Println(fmt.Errorf("unable to connect to RPC server: %v", err))
		// display the error to the web page
		redirectWithError(w, r, "/config?", err)
		return
	}
	defer cleanup()

	// this method will fail if peerswapd is not running or misconfigured
	res, err := client.ReloadPolicyFile(ctx, &peerswaprpc.ReloadPolicyFileRequest{})
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}

	allowlistedPeers := res.GetAllowlistedPeers()
	suspiciousPeers := res.GetSuspiciousPeerList()

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

	res4, err := client.LiquidGetBalance(ctx, &peerswaprpc.GetBalanceRequest{})
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}

	satAmount := res4.GetSatAmount()

	//check for error message to display
	message := ""
	keys, ok := r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		message = keys[0]
	}

	type Page struct {
		AllowSwapRequests bool
		BitcoinSwaps      bool
		Message           string
		ColorScheme       string
		LiquidBalance     uint64
		ListPeers         string
		ListSwaps         string
		BitcoinBalance    uint64
	}

	data := Page{
		AllowSwapRequests: config.AllowSwapRequests,
		BitcoinSwaps:      config.BitcoinSwaps,
		Message:           message,
		ColorScheme:       config.ColorScheme,
		LiquidBalance:     satAmount,
		ListPeers:         convertPeersToHTMLTable(peers, allowlistedPeers, suspiciousPeers),
		ListSwaps:         convertSwapsToHTMLTable(swaps),
		BitcoinBalance:    uint64(lndConfirmedWalletBalance()),
	}

	// executing template named "homepage"
	err = templates.ExecuteTemplate(w, "homepage", data)
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}
}

func peerHandler(w http.ResponseWriter, r *http.Request) {
	keys, ok := r.URL.Query()["id"]
	if !ok || len(keys[0]) < 1 {
		fmt.Println("URL parameter 'id' is missing")
		http.Error(w, http.StatusText(500), 500)
		return
	}

	id := keys[0]
	host := config.RpcHost
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
		redirectWithError(w, r, "/config?", err)
		return
	}
	peers := res.GetPeers()
	peer := findPeerById(peers, id)

	if peer == nil {
		log.Printf("unable to find peer by id: %v", id)
		redirectWithError(w, r, "/config?", errors.New("unable to find peer by id"))
		return
	}

	var sumLocal uint64
	var sumRemote uint64
	for _, ch := range peer.Channels {
		sumLocal += ch.LocalBalance
		sumRemote += ch.RemoteBalance
	}

	res2, err := client.ReloadPolicyFile(ctx, &peerswaprpc.ReloadPolicyFileRequest{})
	if err != nil {
		log.Printf("unable to connect to RPC server: %v", err)
		redirectWithError(w, r, "/config?", err)
		return
	}
	allowlistedPeers := res2.GetAllowlistedPeers()
	suspiciousPeers := res2.GetSuspiciousPeerList()

	res3, err := client.LiquidGetBalance(ctx, &peerswaprpc.GetBalanceRequest{})
	if err != nil {
		log.Printf("unable to connect to RPC server: %v", err)
		redirectWithError(w, r, "/config?", err)
		return
	}

	satAmount := res3.GetSatAmount()

	res4, err := client.ListActiveSwaps(ctx, &peerswaprpc.ListSwapsRequest{})
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}

	activeSwaps := res4.GetSwaps()

	//check for error message to display
	message := ""
	keys, ok = r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		message = keys[0]
	}

	type Page struct {
		Message        string
		ColorScheme    string
		Peer           *peerswaprpc.PeerSwapPeer
		PeerAlias      string
		NodeUrl        string
		Allowed        bool
		Suspicious     bool
		LBTC           bool
		BTC            bool
		LiquidBalance  uint64
		BitcoinBalance uint64
		ActiveSwaps    string
		DirectionIn    bool
	}

	data := Page{
		Message:        message,
		ColorScheme:    config.ColorScheme,
		Peer:           peer,
		PeerAlias:      getNodeAlias(peer.NodeId),
		NodeUrl:        config.NodeApi,
		Allowed:        stringIsInSlice(peer.NodeId, allowlistedPeers),
		Suspicious:     stringIsInSlice(peer.NodeId, suspiciousPeers),
		BTC:            stringIsInSlice("btc", peer.SupportedAssets),
		LBTC:           stringIsInSlice("lbtc", peer.SupportedAssets),
		LiquidBalance:  satAmount,
		BitcoinBalance: uint64(lndConfirmedWalletBalance()),
		ActiveSwaps:    convertSwapsToHTMLTable(activeSwaps),
		DirectionIn:    sumLocal < sumRemote,
	}

	// executing template named "peer"
	err = templates.ExecuteTemplate(w, "peer", data)
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}
}

func swapHandler(w http.ResponseWriter, r *http.Request) {
	keys, ok := r.URL.Query()["id"]
	if !ok || len(keys[0]) < 1 {
		fmt.Println("URL parameter 'id' is missing")
		http.Error(w, http.StatusText(500), 500)
		return
	}
	id := keys[0]

	type Page struct {
		ColorScheme string
		Id          string
		Message     string
	}

	data := Page{
		ColorScheme: config.ColorScheme,
		Id:          id,
		Message:     "",
	}

	// executing template named "swap"
	err := templates.ExecuteTemplate(w, "swap", data)
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}
}

// Updates swap page live
func updateHandler(w http.ResponseWriter, r *http.Request) {
	keys, ok := r.URL.Query()["id"]
	if !ok || len(keys[0]) < 1 {
		fmt.Println("URL parameter 'id' is missing")
		http.Error(w, http.StatusText(500), 500)
		return
	}

	id := keys[0]
	host := config.RpcHost
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

	url := config.BitcoinApi + "/tx/"
	if swap.Asset == "lbtc" {
		url = config.LiquidApi + "/tx/"
	}
	swapData := `<div class="container">
	<div class="columns">
	  <div class="column">
		<div class="box">
		  <table style="table-layout:fixed; width: 100%;">
				<tr>
			  <td style="float: left; text-align: left; width: 80%;">
				<h3 class="title is-4">Swap Details</h3>
			  </td>
			  </td><td style="float: right; text-align: right; width:20%;">
				<h3 class="title is-4">`
	swapData += visualiseSwapStatus(swap.State, true)
	swapData += `</h3>
			  </td>
			</tr>
		  <table>
		  <table style="table-layout:fixed; width: 100%">
			<tr><td style="width:30%; text-align: right">ID:</td><td style="overflow-wrap: break-word;">`
	swapData += swap.Id
	swapData += `</td></tr>
			<tr><td style="text-align: right">Created At:</td><td >`
	swapData += time.Unix(swap.CreatedAt, 0).UTC().Format("2006-01-02 15:04:05")
	swapData += `</td></tr>
			<tr><td style="text-align: right">Asset:</td><td>`
	swapData += swap.Asset
	swapData += `</td></tr>
			<tr><td style="text-align: right">Type:</td><td>`
	swapData += swap.Type
	swapData += `</td></tr>
			<tr><td style="text-align: right">Role:</td><td>`
	swapData += swap.Role
	swapData += `</td></tr>
			<tr><td style="text-align: right">State:</td><td style="overflow-wrap: break-word;">`
	swapData += swap.State
	swapData += `</td></tr>
			<tr><td style="text-align: right">Initiator:</td><td style="overflow-wrap: break-word;">`
	swapData += getNodeAlias(swap.InitiatorNodeId)
	swapData += `&nbsp<a href="`
	swapData += config.NodeApi + "/" + swap.InitiatorNodeId
	swapData += `" target="_blank">🔗</a></td></tr>
			<tr><td style="text-align: right">Peer:</td><td style="overflow-wrap: break-word;">`
	swapData += getNodeAlias(swap.PeerNodeId)
	swapData += `&nbsp<a href="`
	swapData += config.NodeApi + "/" + swap.PeerNodeId
	swapData += `" target="_blank">🔗</a></td></tr>
			<tr><td style="text-align: right">Amount:</td><td>`
	swapData += formatWithThousandSeparators(swap.Amount)
	swapData += `</td></tr>
			<tr><td style="text-align: right">ChannelId:</td><td>`
	swapData += swap.ChannelId
	swapData += `</td></tr>`
	if swap.OpeningTxId != "" {
		swapData += `<tr><td style="text-align: right">OpeningTxId:</td><td style="overflow-wrap: break-word;">`
		swapData += swap.OpeningTxId
		swapData += `&nbsp<a href="`
		swapData += url + swap.OpeningTxId
		swapData += `" target="_blank">🔗</a>`
	}
	if swap.ClaimTxId != "" {
		swapData += `</td></tr>
			<tr><td style="text-align: right">ClaimTxId:</td><td style="overflow-wrap: break-word;">`
		swapData += swap.ClaimTxId
		swapData += `&nbsp<a href="`
		swapData += url + swap.ClaimTxId
		swapData += `" target="_blank">🔗</a></td></tr>`
	}
	if swap.CancelMessage != "" {
		swapData += `<tr><td style="text-align: right">CancelMsg:</td><td>`
		swapData += swap.CancelMessage
		swapData += `</td></tr>`
	}
	swapData += `<tr><td style="text-align: right">LndChanId:</td><td>`
	swapData += strconv.FormatUint(uint64(swap.LndChanId), 10)
	swapData += `</td></tr>
		  </table>
		</div>
	  </div>
	</div>
  </div>`

	// Send the updated data as the response
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(swapData))
}

func configHandler(w http.ResponseWriter, r *http.Request) {
	//check for error message to display
	message := ""
	keys, ok := r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		message = keys[0]
	}

	type Page struct {
		Message     string
		ColorScheme string
		Config      Configuration
		Version     string
		Latest      string
	}

	data := Page{
		Message:     message,
		ColorScheme: config.ColorScheme,
		Config:      config,
		Version:     version,
		Latest:      getLatestTag(),
	}

	// executing template named "error"
	err := templates.ExecuteTemplate(w, "config", data)
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}
}

func liquidHandler(w http.ResponseWriter, r *http.Request) {
	//check for error message to display
	message := ""
	keys, ok := r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		message = keys[0]
	}

	txid := ""
	keys, ok = r.URL.Query()["txid"]
	if ok && len(keys[0]) > 0 {
		txid = keys[0]
	}

	addr := ""
	keys, ok = r.URL.Query()["addr"]
	if ok && len(keys[0]) > 0 {
		addr = keys[0]
	}

	host := config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		log.Printf("unable to connect to RPC server: %v", err)
		redirectWithError(w, r, "/?", err)
		return
	}
	defer cleanup()

	res2, err := client.LiquidGetBalance(ctx, &peerswaprpc.GetBalanceRequest{})
	if err != nil {
		log.Printf("unable to connect to RPC server: %v", err)
		redirectWithError(w, r, "/?", err)
		return
	}

	var outputs []LiquidUTXO

	if err := listUnspent(&outputs); err != nil {
		log.Printf("unable get listUnspent: %v", err)
		redirectWithError(w, r, "/liquid?", err)
		return
	}

	// sort outputs on Confirmations field
	sort.Slice(outputs, func(i, j int) bool {
		return outputs[i].Confirmations < outputs[j].Confirmations
	})

	type Page struct {
		Message       string
		ColorScheme   string
		LiquidAddress string
		LiquidBalance uint64
		TxId          string
		LiquidUrl     string
		Outputs       *[]LiquidUTXO
		LiquidApi     string
	}

	data := Page{
		Message:       message,
		ColorScheme:   config.ColorScheme,
		LiquidAddress: addr,
		LiquidBalance: res2.GetSatAmount(),
		TxId:          txid,
		LiquidUrl:     config.LiquidApi + "/tx/" + txid,
		Outputs:       &outputs,
		LiquidApi:     config.LiquidApi,
	}

	// executing template named "liquid"
	err = templates.ExecuteTemplate(w, "liquid", data)
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}
}

func submitHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// Parse the form data
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Error parsing form data", http.StatusBadRequest)
			return
		}

		action := r.FormValue("action")
		nodeId := r.FormValue("nodeId")
		host := config.RpcHost
		ctx := context.Background()

		client, cleanup, err := getClient(host)
		if err != nil {
			redirectWithError(w, r, "/config?", err)
			return
		}
		defer cleanup()

		switch action {
		case "newAddress":
			res, err := client.LiquidGetAddress(ctx, &peerswaprpc.GetAddressRequest{})
			if err != nil {
				log.Printf("unable to connect to RPC server: %v", err)
				redirectWithError(w, r, "/liquid?", err)
				return
			}

			// Redirect to liquid page with new address
			http.Redirect(w, r, "/liquid?msg=\"\"&addr="+res.Address, http.StatusSeeOther)
			return

		case "sendLiquid":
			amt, err := strconv.ParseUint(r.FormValue("sendAmount"), 10, 64)
			if err != nil {
				redirectWithError(w, r, "/liquid?", err)
				return
			}

			txid, err := sendLiquidToAddress(
				r.FormValue("sendAddress"),
				amt,
				r.FormValue("subtractfee") == "on")
			if err != nil {
				redirectWithError(w, r, "/liquid?", err)
				return
			}

			// Redirect to liquid page with TxId
			http.Redirect(w, r, "/liquid?msg=\"\"&txid="+txid, http.StatusSeeOther)
			return
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

		case "suspectPeer":
			_, err := client.AddSusPeer(ctx, &peerswaprpc.AddPeerRequest{
				PeerPubkey: nodeId,
			})
			if err != nil {
				redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
				return
			}
			// Redirect to peer page
			http.Redirect(w, r, "/peer?id="+nodeId, http.StatusSeeOther)
			return

		case "unsuspectPeer":
			_, err := client.RemoveSusPeer(ctx, &peerswaprpc.RemovePeerRequest{
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
					Force:      r.FormValue("force") == "on",
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
					Force:      r.FormValue("force") == "on",
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

// saves config
func saveConfigHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// Parse the form data
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Error parsing form data", http.StatusBadRequest)
			return
		}

		allowSwapRequests, err := strconv.ParseBool(r.FormValue("allowSwapRequests"))
		if err != nil {
			redirectWithError(w, r, "/config?", err)
			return
		}

		config.ColorScheme = r.FormValue("colorScheme")
		config.NodeApi = r.FormValue("nodeApi")
		config.BitcoinApi = r.FormValue("bitcoinApi")
		config.LiquidApi = r.FormValue("liquidApi")

		if config.TelegramToken != r.FormValue("telegramToken") {
			config.TelegramToken = r.FormValue("telegramToken")
			go telegramStart()
		}

		if config.LocalMempool != r.FormValue("localMempool") && r.FormValue("localMempool") != "" {
			// update bitcoinApi link
			config.BitcoinApi = r.FormValue("localMempool")
		}

		config.LocalMempool = r.FormValue("localMempool")

		bitcoinSwaps, err := strconv.ParseBool(r.FormValue("bitcoinSwaps"))
		if err != nil {
			bitcoinSwaps = false
		}

		mustRestart := false
		if config.BitcoinSwaps != bitcoinSwaps || config.ElementsUser != r.FormValue("elementsUser") || config.ElementsPass != r.FormValue("elementsPass") {
			mustRestart = true
		}

		config.BitcoinSwaps = bitcoinSwaps
		config.ElementsUser = r.FormValue("elementsUser")
		config.ElementsPass = r.FormValue("elementsPass")
		config.ElementsDir = r.FormValue("elementsDir")
		config.ElementsDirMapped = r.FormValue("elementsDirMapped")
		config.BitcoinHost = r.FormValue("bitcoinHost")
		config.BitcoinUser = r.FormValue("bitcoinUser")
		config.BitcoinPass = r.FormValue("bitcoinPass")

		mh, err := strconv.ParseUint(r.FormValue("maxHistory"), 10, 16)
		if err != nil {
			redirectWithError(w, r, "/config?", err)
			return
		}
		config.MaxHistory = uint(mh)

		ctx := context.Background()
		host := r.FormValue("rpcHost")
		clientIsDown := false
		client, cleanup, err := getClient(host)
		if err != nil {
			clientIsDown = true
		} else {
			defer cleanup()
			_, err = client.AllowSwapRequests(ctx, &peerswaprpc.AllowSwapRequestsRequest{
				Allow: allowSwapRequests,
			})
			if err != nil {
				// RPC Host entered is bad
				clientIsDown = true
			} else { // values are good, save them
				config.RpcHost = host
				config.AllowSwapRequests = allowSwapRequests
			}
		}

		if err2 := saveConfig(); err2 != nil {
			redirectWithError(w, r, "/config?", err2)
			return
		}

		if mustRestart {
			// show progress bar and log
			go http.Redirect(w, r, "/loading", http.StatusSeeOther)
			savePeerSwapdConfig()
			stopPeerSwapd()
		} else if clientIsDown { // configs did not work, try again
			redirectWithError(w, r, "/config?", err)
		} else { // configs are good
			http.Redirect(w, r, "/", http.StatusSeeOther)
		}
	} else {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func stopHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "Stopping PeerSwap...", http.StatusBadGateway)
	log.Println("Stop requested")
	go func() {
		stopPeerSwapd()
		os.Exit(0) // Exit the program
	}()
}

func loadingHandler(w http.ResponseWriter, r *http.Request) {
	type Page struct {
		ColorScheme string
		Message     string
		LogPosition int
		LogFile     string
	}

	data := Page{
		ColorScheme: config.ColorScheme,
		Message:     "",
		LogPosition: 0,     // new content and wait for connection
		LogFile:     "log", // peerswapd log
	}

	// executing template named "loading"
	err := templates.ExecuteTemplate(w, "loading", data)
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}
}

func backupHandler(w http.ResponseWriter, r *http.Request) {
	wallet := getPeerswapConfSetting("elementsd.rpcwallet")
	// returns .bak with the name of the wallet
	if fileName, err := backupAndZip(wallet); err == nil {
		// Set the Content-Disposition header to suggest a filename
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fileName))
		// Serve the file for download
		http.ServeFile(w, r, filepath.Join(config.DataDir, fileName))
		// Delete zip archive
		err = os.Remove(filepath.Join(config.DataDir, fileName))
		if err != nil {
			log.Println("Error deleting zip file:", err)
		}
	} else {
		redirectWithError(w, r, "/liquid?", err)
	}
}

// shows peerswapd log
func logHandler(w http.ResponseWriter, r *http.Request) {
	type Page struct {
		ColorScheme string
		Message     string
		LogPosition int
		LogFile     string
	}

	logFile := "log"

	keys, ok := r.URL.Query()["log"]
	if ok && len(keys[0]) > 0 {
		logFile = keys[0]
	}

	data := Page{
		ColorScheme: config.ColorScheme,
		Message:     "",
		LogPosition: 1, // from first line
		LogFile:     logFile,
	}

	// executing template named "logpage"
	err := templates.ExecuteTemplate(w, "logpage", data)
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}
}

// returns log as JSON
func logApiHandler(w http.ResponseWriter, r *http.Request) {

	logText := ""

	keys, ok := r.URL.Query()["pos"]
	if !ok || len(keys[0]) < 1 {
		log.Println("URL parameter 'pos' is missing")
		w.WriteHeader(http.StatusOK)
		return
	}

	startPosition, err := strconv.ParseInt(keys[0], 10, 64)
	if err != nil {
		log.Println("Error:", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	logFile := "log"

	keys, ok = r.URL.Query()["log"]
	if ok && len(keys[0]) > 0 {
		logFile = keys[0]
	}

	filename := filepath.Join(config.DataDir, logFile)

	file, err := os.Open(filename)
	if err != nil {
		log.Println("Error opening file:", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		log.Println("Error getting file info:", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	fileSize := fileInfo.Size()

	if startPosition > 0 && fileSize > startPosition {
		// Seek to the desired starting position
		_, err = file.Seek(startPosition, 0)
		if err != nil {
			log.Println("Error seeking:", err)
			w.WriteHeader(http.StatusOK)
			return
		}

		// Read from the current position till EOF
		content, err := io.ReadAll(file)
		if err != nil {
			log.Println("Error reading file:", err)
			w.WriteHeader(http.StatusOK)
			return
		}

		logText = (string(content))
		length := len(logText)

		if startPosition == 1 && length > 10000 {
			// limit to 10000 characters
			logText = logText[length-10000:]
		}
	}

	// Create a response struct
	type ResponseData struct {
		NextPosition int64
		LogText      string
	}

	responseData := ResponseData{
		NextPosition: fileSize,
		LogText:      logText,
	}

	// Marshal the response struct to JSON
	responseJSON, err := json.Marshal(responseData)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Send the next chunk of the log as the response
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(responseJSON))
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

func startTimer() {
	for range time.Tick(60 * time.Second) {
		liquidBackup(false)
		if config.PeginTxId != "" {
			confs := lndNumConfirmations(config.PeginTxId)
			if confs >= 102 {
				rawTx, err := getRawTransaction(config.PeginTxId)
				if err == nil {
					proof := getTxOutProof(config.PeginTxId)
					txid, err := claimPegin(rawTx, proof, config.PeginClaimScript)

					// claimpegin takes long time, allow it to timeout
					if err != nil && err.Error() != "timeout reading data from server" {
						log.Println("Peg-in claim FAILED!")
						log.Println("Mainchain TxId:", config.PeginTxId)
						log.Println("Raw tx:", rawTx)
						log.Println("Proof:", proof)
						log.Println("Claim Script:", config.PeginClaimScript)
						telegramSendMessage("❗ Peg-in claim FAILED! See log for details.")
					} else {
						log.Println("Peg-in success! Liquid TxId:", txid)
						telegramSendMessage("💸 Peg-in success!")
					}
				} else {
					log.Println("Peg-In getrawtx FAILED.")
					log.Println("Mainchain TxId:", config.PeginTxId)
					log.Println("Claim Script:", config.PeginClaimScript)
					telegramSendMessage("❗ Peg-In getrawtx FAILED! See log for details.")
				}

				// stop trying after first attempt
				config.PeginTxId = ""
				saveConfig()
			}
		}

	}
}

func liquidBackup(force bool) {
	// skip backup if missing RPC or Telegram credentials
	if config.ElementsPass == "" || config.ElementsUser == "" || chatId == 0 {
		return
	}

	host := config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		log.Println(fmt.Errorf("unable to connect to RPC server: %v", err))
		// display the error to the web page
		return
	}
	defer cleanup()

	res, err := client.ListActiveSwaps(ctx, &peerswaprpc.ListSwapsRequest{})
	if err != nil {
		return
	}

	// do not backup while a swap is pending
	if len(res.GetSwaps()) > 0 && !force {
		return
	}

	res2, err := client.LiquidGetBalance(ctx, &peerswaprpc.GetBalanceRequest{})
	if err != nil {
		return
	}

	satAmount := res2.GetSatAmount()

	// do not backup if the sat amount did not change
	if satAmount == config.ElementsBackupAmount && !force {
		return
	}

	wallet := getPeerswapConfSetting("elementsd.rpcwallet")
	destinationZip, err := backupAndZip(wallet)
	if err != nil {
		log.Println("Error zipping backup:", err)
		return
	}

	err = telegramSendFile(config.DataDir, destinationZip, formatWithThousandSeparators(satAmount))
	if err != nil {
		log.Println("Error sending zip:", err)
		return
	}

	// Delete zip archive
	err = os.Remove(filepath.Join(config.DataDir, destinationZip))
	if err != nil {
		log.Println("Error deleting zip file:", err)
	}

	// save the wallet amount
	config.ElementsBackupAmount = satAmount
	saveConfig()
}

func bitcoinHandler(w http.ResponseWriter, r *http.Request) {
	//check for error message to display
	message := ""
	keys, ok := r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		message = keys[0]
	}

	utxos := lndListUnspent()

	type Page struct {
		Message        string
		ColorScheme    string
		BitcoinBalance uint64
		Outputs        []*lnrpc.Utxo
		PeginTxId      string
		BitcoinApi     string
		Confirmations  int32
		Progress       int32
		ETA            string
	}

	btcBalance := lndConfirmedWalletBalance()
	confs := int32(0)

	if config.PeginTxId != "" {
		confs = lndNumConfirmations(config.PeginTxId)
	}

	n := (102 - confs) * 10
	futureTime := time.Now().Add(time.Duration(n) * time.Minute)
	eta := futureTime.Format("15:04")

	data := Page{
		Message:        message,
		ColorScheme:    config.ColorScheme,
		BitcoinBalance: uint64(btcBalance),
		Outputs:        utxos,
		PeginTxId:      config.PeginTxId,
		BitcoinApi:     config.BitcoinApi,
		Confirmations:  confs,
		Progress:       int32(confs * 100 / 102),
		ETA:            eta,
	}

	// executing template named "bitcoin"
	err := templates.ExecuteTemplate(w, "bitcoin", data)
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}
}

func peginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// Parse the form data
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Error parsing form data", http.StatusBadRequest)
			return
		}

		amount, err := strconv.ParseInt(r.FormValue("peginAmount"), 10, 64)
		if err != nil {
			redirectWithError(w, r, "/bitcoin?", err)
			return
		}

		fee, err := strconv.ParseUint(r.FormValue("feeRate"), 10, 64)
		if err != nil {
			redirectWithError(w, r, "/bitcoin?", err)
			return
		}

		btcBalance := lndConfirmedWalletBalance()
		sweepall := amount == btcBalance

		// test that txindex=1 in bitcoin.conf
		tx := "b61ec844027ce18fd3eb91fa7bed8abaa6809c4d3f6cf4952b8ebaa7cd46583a"
		if os.Getenv("NETWORK") == "testnet" {
			tx = "2c7ec5043fe8ee3cb4ce623212c0e52087d3151c9e882a04073cce1688d6fc1e"
		}

		_, err = getRawTransaction(tx)
		if err != nil {
			// fallback to getblock.io
			config.BitcoinHost = getBlockIoHost()
			config.BitcoinUser = ""
			config.BitcoinPass = ""
			_, err = getRawTransaction(tx)
			if err != nil {
				redirectWithError(w, r, "/bitcoin?", errors.New("getrawtransaction request failed, check BitcoinHost in Config"))
				return
			} else {
				// use getblock.io endpoint going forward
				saveConfig()
			}
		}

		var addr PeginAddress

		err = getPeginAddress(&addr)
		if err != nil {
			redirectWithError(w, r, "/bitcoin?", err)
			return
		}

		log.Println("Peg-in started to mainchain address:", addr.MainChainAddress, "claim script:", addr.ClaimScript, "amount:", amount)
		futureTime := time.Now().Add(time.Duration(1010) * time.Minute)
		eta := futureTime.Format("15:04")
		telegramSendMessage("⏰ Peg-in started. ETA: " + eta)

		config.PeginClaimScript = addr.ClaimScript
		saveConfig()

		txid, err := lndSendCoins(addr.MainChainAddress, amount, fee, sweepall, "Liquid pegin")
		if err != nil {
			redirectWithError(w, r, "/bitcoin?", err)
			return
		}

		config.PeginTxId = txid
		saveConfig()

		// Redirect to bitcoin page to follow the pegin progress
		http.Redirect(w, r, "/bitcoin", http.StatusSeeOther)
	} else {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}
