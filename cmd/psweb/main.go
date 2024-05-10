package main

import (
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
	"strings"
	"syscall"
	"text/template"
	"time"

	"peerswap-web/cmd/psweb/bitcoin"
	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/internet"
	"peerswap-web/cmd/psweb/liquid"
	"peerswap-web/cmd/psweb/ln"
	"peerswap-web/cmd/psweb/ps"

	"github.com/elementsproject/peerswap/peerswaprpc"
	"github.com/gorilla/mux"
)

const version = "v1.3.5"

type AliasCache struct {
	PublicKey string
	Alias     string
}

var (
	aliasCache []AliasCache
	templates  = template.New("")
	//go:embed static
	staticFiles embed.FS
	//go:embed templates/*.gohtml
	tplFolder      embed.FS
	logFile        *os.File
	latestVersion  = version
	mempoolFeeRate = float64(0)
)

func main() {

	var (
		dataDir     = flag.String("datadir", "", "Path to config folder")
		showHelp    = flag.Bool("help", false, "Show help")
		showVersion = flag.Bool("version", false, "Show version")
	)

	flag.Parse()

	// loading from config file or creating default one
	config.Load(*dataDir)

	if *showHelp {
		showHelpMessage()
		return
	}

	if *showVersion {
		showVersionInfo()
		return
	}

	// set logging params
	cleanup, err := setLogging()
	if err != nil {
		log.Fatal(err)
	}
	defer cleanup()

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
	r.HandleFunc("/bumpfee", bumpfeeHandler)

	// Start the server
	http.Handle("/", r)
	go func() {
		if err := http.ListenAndServe(":"+config.Config.ListenPort, nil); err != nil {
			log.Fatal(err)
		}
	}()

	log.Println("Listening on http://localhost:" + config.Config.ListenPort)

	// Start timer to run every minute
	go startTimer()

	// to speed up first load of home page
	go cacheAliases()

	// Handle termination signals
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for termination signal
	<-signalChan
	log.Println("Received termination signal")

	// Exit the program gracefully
	os.Exit(0)
}

func indexHandler(w http.ResponseWriter, r *http.Request) {

	if config.Config.ElementsPass == "" || config.Config.ElementsUser == "" {
		http.Redirect(w, r, "/config?err=welcome", http.StatusSeeOther)
		return
	}

	// this method will fail if peerswap is not running or misconfigured
	client, cleanup, err := ps.GetClient(config.Config.RpcHost)
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}
	defer cleanup()

	res, err := ps.ReloadPolicyFile(client)
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}

	allowlistedPeers := res.GetAllowlistedPeers()
	suspiciousPeers := res.GetSuspiciousPeerList()

	res2, err := ps.ListPeers(client)
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}
	peers := res2.GetPeers()

	res3, err := ps.ListSwaps(client)
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}
	swaps := res3.GetSwaps()

	res4, err := ps.LiquidGetBalance(client)
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}

	satAmount := res4.GetSatAmount()

	cl, clean, er := ln.GetClient()
	if er != nil {
		redirectWithError(w, r, "/config?", er)
		return
	}
	defer clean()

	btcBalance := ln.ConfirmedWalletBalance(cl)

	//check for error message to display
	message := ""
	keys, ok := r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		message = keys[0]
	}

	//check for node Id to filter swaps
	nodeId := ""
	keys, ok = r.URL.Query()["id"]
	if ok && len(keys[0]) > 0 {
		nodeId = keys[0]
	}

	//check for swaps state to filter
	state := ""
	keys, ok = r.URL.Query()["state"]
	if ok && len(keys[0]) > 0 {
		state = keys[0]
	}

	//check for swaps role to filter
	role := ""
	keys, ok = r.URL.Query()["role"]
	if ok && len(keys[0]) > 0 {
		role = keys[0]
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
		Filter            bool
		MempoolFeeRate    float64
	}

	data := Page{
		AllowSwapRequests: config.Config.AllowSwapRequests,
		BitcoinSwaps:      config.Config.BitcoinSwaps,
		Message:           message,
		MempoolFeeRate:    mempoolFeeRate,
		ColorScheme:       config.Config.ColorScheme,
		LiquidBalance:     satAmount,
		ListPeers:         convertPeersToHTMLTable(peers, allowlistedPeers, suspiciousPeers, swaps),
		ListSwaps:         convertSwapsToHTMLTable(swaps, nodeId, state, role),
		BitcoinBalance:    uint64(btcBalance),
		Filter:            nodeId != "" || state != "" || role != "",
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

	client, cleanup, err := ps.GetClient(config.Config.RpcHost)
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}
	defer cleanup()

	res, err := ps.ListPeers(client)
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

	res2, err := ps.ReloadPolicyFile(client)
	if err != nil {
		log.Printf("unable to connect to RPC server: %v", err)
		redirectWithError(w, r, "/config?", err)
		return
	}
	allowlistedPeers := res2.GetAllowlistedPeers()
	suspiciousPeers := res2.GetSuspiciousPeerList()

	res3, err := ps.LiquidGetBalance(client)
	if err != nil {
		log.Printf("unable to connect to RPC server: %v", err)
		redirectWithError(w, r, "/config?", err)
		return
	}

	satAmount := res3.GetSatAmount()

	res4, err := ps.ListActiveSwaps(client)
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}

	activeSwaps := res4.GetSwaps()

	cl, clean, er := ln.GetClient()
	if er != nil {
		redirectWithError(w, r, "/config?", er)
		return
	}
	defer clean()

	btcBalance := ln.ConfirmedWalletBalance(cl)

	var sumLocal uint64
	var sumRemote uint64
	var stats []*ln.ForwardingStats
	var channelInfo []*ln.ChanneInfo

	for _, ch := range peer.Channels {
		stat := ln.GetForwardingStats(ch.ChannelId)
		stats = append(stats, stat)

		info := ln.GetChannelInfo(cl, ch.ChannelId, peer.NodeId)
		info.LocalBalance = ch.GetLocalBalance()
		info.RemoteBalance = ch.GetRemoteBalance()
		info.Active = ch.GetActive()
		channelInfo = append(channelInfo, info)

		sumLocal += ch.GetLocalBalance()
		sumRemote += ch.GetRemoteBalance()
	}

	//check for error message to display
	message := ""
	keys, ok = r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		message = keys[0]
	}

	// get routing stats

	type Page struct {
		Message        string
		MempoolFeeRate float64
		BtcFeeRate     float64
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
		Stats          []*ln.ForwardingStats
		ChannelInfo    []*ln.ChanneInfo
	}

	data := Page{
		Message:        message,
		BtcFeeRate:     mempoolFeeRate,
		MempoolFeeRate: liquid.GetMempoolMinFee(),
		ColorScheme:    config.Config.ColorScheme,
		Peer:           peer,
		PeerAlias:      getNodeAlias(peer.NodeId),
		NodeUrl:        config.Config.NodeApi,
		Allowed:        stringIsInSlice(peer.NodeId, allowlistedPeers),
		Suspicious:     stringIsInSlice(peer.NodeId, suspiciousPeers),
		BTC:            stringIsInSlice("btc", peer.SupportedAssets),
		LBTC:           stringIsInSlice("lbtc", peer.SupportedAssets),
		LiquidBalance:  satAmount,
		BitcoinBalance: uint64(btcBalance),
		ActiveSwaps:    convertSwapsToHTMLTable(activeSwaps, "", "", ""),
		DirectionIn:    sumLocal < sumRemote,
		Stats:          stats,
		ChannelInfo:    channelInfo,
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

	client, cleanup, err := ps.GetClient(config.Config.RpcHost)
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}
	defer cleanup()

	res, err := ps.GetSwap(client, id)
	if err != nil {
		redirectWithError(w, r, "/swap?id="+id+"&", err)
		return
	}

	swap := res.GetSwap()

	isPending := true

	switch swap.State {
	case "State_ClaimedCoop",
		"State_ClaimedCsv",
		"State_SwapCanceled",
		"State_SendCancel",
		"State_ClaimedPreimage":
		isPending = false
	}

	type Page struct {
		ColorScheme    string
		Id             string
		Message        string
		MempoolFeeRate float64
		IsPending      bool
	}

	data := Page{
		ColorScheme:    config.Config.ColorScheme,
		Id:             id,
		Message:        "",
		MempoolFeeRate: mempoolFeeRate,
		IsPending:      isPending,
	}

	// executing template named "swap"
	err = templates.ExecuteTemplate(w, "swap", data)
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

	client, cleanup, err := ps.GetClient(config.Config.RpcHost)
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}
	defer cleanup()

	res, err := ps.GetSwap(client, id)
	if err != nil {
		log.Printf("onSwap: %v", err)
		redirectWithError(w, r, "/swap?id="+id+"&", err)
		return
	}

	swap := res.GetSwap()

	url := config.Config.BitcoinApi + "/tx/"
	if swap.Asset == "lbtc" {
		url = config.Config.LiquidApi + "/tx/"
	}
	swapData := `<div class="container">
	<div class="columns">
	  <div class="column">
		<div class="box">
		  <table style="table-layout:fixed; width: 100%;">
				<tr>
			  <td style="float: left; text-align: left; width: 80%;">
				<h4 class="title is-4">Swap Details</h4>
			  </td>
			  </td><td style="float: right; text-align: right; width:20%;">
				<h4 class="title is-4"><a title="Return to initial page" href="/">`
	swapData += visualiseSwapState(swap.State, true)
	swapData += `</a></h4>
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
	swapData += config.Config.NodeApi + "/" + swap.InitiatorNodeId
	swapData += `" target="_blank">🔗</a></td></tr>
			<tr><td style="text-align: right">Peer:</td><td style="overflow-wrap: break-word;">`
	swapData += getNodeAlias(swap.PeerNodeId)
	swapData += `&nbsp<a href="`
	swapData += config.Config.NodeApi + "/" + swap.PeerNodeId
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
		Message        string
		MempoolFeeRate float64
		ColorScheme    string
		Config         config.Configuration
		Version        string
		Latest         string
		Implementation string
	}

	data := Page{
		Message:        message,
		MempoolFeeRate: mempoolFeeRate,
		ColorScheme:    config.Config.ColorScheme,
		Config:         config.Config,
		Version:        version,
		Latest:         latestVersion,
		Implementation: ln.Implementation,
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

	client, cleanup, err := ps.GetClient(config.Config.RpcHost)
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}
	defer cleanup()

	res2, err := ps.LiquidGetBalance(client)
	if err != nil {
		log.Printf("unable to connect to RPC server: %v", err)
		redirectWithError(w, r, "/?", err)
		return
	}

	var outputs []liquid.UTXO

	if err := liquid.ListUnspent(&outputs); err != nil {
		log.Printf("unable get listUnspent: %v", err)
		redirectWithError(w, r, "/liquid?", err)
		return
	}

	// sort outputs on Confirmations field
	sort.Slice(outputs, func(i, j int) bool {
		return outputs[i].Confirmations < outputs[j].Confirmations
	})

	type Page struct {
		Message        string
		MempoolFeeRate float64
		ColorScheme    string
		LiquidAddress  string
		LiquidBalance  uint64
		TxId           string
		LiquidUrl      string
		Outputs        *[]liquid.UTXO
		LiquidApi      string
	}

	data := Page{
		Message:        message,
		MempoolFeeRate: liquid.GetMempoolMinFee(),
		ColorScheme:    config.Config.ColorScheme,
		LiquidAddress:  addr,
		LiquidBalance:  res2.GetSatAmount(),
		TxId:           txid,
		LiquidUrl:      config.Config.LiquidApi + "/tx/" + txid,
		Outputs:        &outputs,
		LiquidApi:      config.Config.LiquidApi,
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

		client, cleanup, err := ps.GetClient(config.Config.RpcHost)
		if err != nil {
			redirectWithError(w, r, "/config?", err)
			return
		}
		defer cleanup()

		switch action {
		case "newAddress":
			res, err := ps.LiquidGetAddress(client)
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

			txid, err := liquid.SendToAddress(
				r.FormValue("sendAddress"),
				amt,
				r.FormValue("comment"),
				r.FormValue("subtractfee") == "on",
				true,
				r.FormValue("ignoreblindfail") == "on")
			if err != nil {
				redirectWithError(w, r, "/liquid?", err)
				return
			}

			// Redirect to liquid page with TxId
			http.Redirect(w, r, "/liquid?msg=\"\"&txid="+txid, http.StatusSeeOther)
			return
		case "addPeer":
			_, err := ps.AddPeer(client, nodeId)
			if err != nil {
				redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
				return
			}
			// Redirect to peer page
			http.Redirect(w, r, "/peer?id="+nodeId, http.StatusSeeOther)
			return

		case "removePeer":
			_, err := ps.RemovePeer(client, nodeId)
			if err != nil {
				redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
				return
			}
			// Redirect to peer page
			http.Redirect(w, r, "/peer?id="+nodeId, http.StatusSeeOther)
			return

		case "suspectPeer":
			_, err := ps.AddSusPeer(client, nodeId)
			if err != nil {
				redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
				return
			}
			// Redirect to peer page
			http.Redirect(w, r, "/peer?id="+nodeId, http.StatusSeeOther)
			return

		case "unsuspectPeer":
			_, err := ps.RemoveSusPeer(client, nodeId)
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
				id, err := ps.SwapIn(client, swapAmount, channelId, r.FormValue("asset"), r.FormValue("force") == "on")
				if err != nil {
					redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
					return
				}
				// Redirect to swap page to follow the swap
				http.Redirect(w, r, "/swap?id="+id, http.StatusSeeOther)

			case "swapOut":
				id, err := ps.SwapOut(client, swapAmount, channelId, r.FormValue("asset"), r.FormValue("force") == "on")
				if err != nil {
					redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
					return
				}
				// Redirect to swap page to follow the swap
				http.Redirect(w, r, "/swap?id="+id, http.StatusSeeOther)
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

		config.Config.ColorScheme = r.FormValue("colorScheme")
		config.Config.NodeApi = r.FormValue("nodeApi")
		config.Config.BitcoinApi = r.FormValue("bitcoinApi")
		config.Config.LiquidApi = r.FormValue("liquidApi")

		if config.Config.TelegramToken != r.FormValue("telegramToken") {
			config.Config.TelegramToken = r.FormValue("telegramToken")
			go telegramStart()
		}

		if config.Config.LocalMempool != r.FormValue("localMempool") && r.FormValue("localMempool") != "" {
			// update bitcoinApi link
			config.Config.BitcoinApi = r.FormValue("localMempool")
		}

		config.Config.LocalMempool = r.FormValue("localMempool")

		bitcoinSwaps, err := strconv.ParseBool(r.FormValue("bitcoinSwaps"))
		if err != nil {
			bitcoinSwaps = false
		}

		mustRestart := false
		if config.Config.BitcoinSwaps != bitcoinSwaps || config.Config.ElementsUser != r.FormValue("elementsUser") || config.Config.ElementsPass != r.FormValue("elementsPass") {
			mustRestart = true
		}

		config.Config.BitcoinSwaps = bitcoinSwaps
		config.Config.ElementsUser = r.FormValue("elementsUser")
		config.Config.ElementsPass = r.FormValue("elementsPass")
		config.Config.ElementsDir = r.FormValue("elementsDir")
		config.Config.ElementsDirMapped = r.FormValue("elementsDirMapped")
		config.Config.BitcoinHost = r.FormValue("bitcoinHost")
		config.Config.BitcoinUser = r.FormValue("bitcoinUser")
		config.Config.BitcoinPass = r.FormValue("bitcoinPass")
		config.Config.ProxyURL = r.FormValue("proxyURL")

		mh, err := strconv.ParseUint(r.FormValue("maxHistory"), 10, 16)
		if err != nil {
			redirectWithError(w, r, "/config?", err)
			return
		}
		config.Config.MaxHistory = uint(mh)

		host := r.FormValue("rpcHost")
		clientIsDown := false

		client, cleanup, err := ps.GetClient(host)
		if err != nil {
			clientIsDown = true
		} else {
			defer cleanup()
			_, err = ps.AllowSwapRequests(client, allowSwapRequests)
			if err != nil {
				// RPC Host entered is bad
				clientIsDown = true
			} else { // values are good, save them
				config.Config.RpcHost = host
				config.Config.AllowSwapRequests = allowSwapRequests
			}
		}

		if err2 := config.Save(); err2 != nil {
			redirectWithError(w, r, "/config?", err2)
			return
		}

		// reset Aliases cache
		aliasCache = []AliasCache{}

		if mustRestart {
			// show progress bar and log
			go http.Redirect(w, r, "/loading", http.StatusSeeOther)
			config.SavePS()
			ps.Stop()
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
		ps.Stop()
		os.Exit(0) // Exit the program
	}()
}

func loadingHandler(w http.ResponseWriter, r *http.Request) {
	type Page struct {
		ColorScheme    string
		Message        string
		MempoolFeeRate float64
		LogPosition    int
		LogFile        string
		SearchText     string
	}

	logFile := "log" // peerswapd log
	searchText := "peerswapd grpc listening on"
	if ln.Implementation == "CLN" {
		logFile = "cln.log"
		searchText = "plugin-peerswap: peerswap initialized"
	}

	data := Page{
		ColorScheme:    config.Config.ColorScheme,
		Message:        "",
		MempoolFeeRate: mempoolFeeRate,
		LogPosition:    0, // new content and wait for connection
		LogFile:        logFile,
		SearchText:     searchText,
	}

	// executing template named "loading"
	err := templates.ExecuteTemplate(w, "loading", data)
	if err != nil {
		log.Fatalln(err)
		http.Error(w, http.StatusText(500), 500)
	}
}

func backupHandler(w http.ResponseWriter, r *http.Request) {
	wallet := config.Config.ElementsWallet
	// returns .bak with the name of the wallet
	if fileName, err := liquid.BackupAndZip(wallet); err == nil {
		// Set the Content-Disposition header to suggest a filename
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fileName))
		// Serve the file for download
		http.ServeFile(w, r, filepath.Join(config.Config.DataDir, fileName))
		// Delete zip archive
		err = os.Remove(filepath.Join(config.Config.DataDir, fileName))
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
		ColorScheme    string
		Message        string
		MempoolFeeRate float64
		LogPosition    int
		LogFile        string
		Implementation string
	}

	logFile := "log"

	keys, ok := r.URL.Query()["log"]
	if ok && len(keys[0]) > 0 {
		logFile = keys[0]
	}

	data := Page{
		ColorScheme:    config.Config.ColorScheme,
		Message:        "",
		MempoolFeeRate: mempoolFeeRate,
		LogPosition:    1, // from first line
		LogFile:        logFile,
		Implementation: ln.Implementation,
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

	filename := filepath.Join(config.Config.DataDir, logFile)

	if logFile == "cln.log" {
		filename = filepath.Join(config.Config.LightningDir, logFile)
	} else if logFile == "lnd.log" {
		filename = filepath.Join(config.Config.LightningDir, "logs", "bitcoin", config.Config.Chain, logFile)
	}

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
	t := fmt.Sprintln(err)
	// translate common errors into plain English
	switch {
	case strings.HasPrefix(t, "rpc error: code = Unavailable desc = connection error"):
		t = "Cannot connect to peerswapd. It either failed to start, awaits LND or has wrong configuration. Check logs."
	case strings.HasPrefix(t, "Unable to dial socket"):
		t = "Cannot connect to lightningd. It either failed to start or has wrong configuration. Check logs."
	case strings.HasPrefix(t, "-32601:Unknown command 'peerswap-reloadpolicy'"):
		t = "Peerswap plugin is not installed or has wrong configuration. Check .lightning/config."
	}
	// display the error to the web page header
	msg := url.QueryEscape(t)
	http.Redirect(w, r, redirectUrl+"err="+msg, http.StatusSeeOther)
}

func showHelpMessage() {
	fmt.Println("A lightweight server-side rendered Web UI for PeerSwap, which allows trustless p2p submarine swaps Lightning<->BTC and Lightning<->Liquid. Also facilitates BTC->Liquid peg-ins. PeerSwap with Liquid is a great cost efficient way to rebalance lightning channels.")
	fmt.Println("Usage:")
	flag.PrintDefaults()
}

func showVersionInfo() {
	fmt.Println("Version:", version, "for", ln.Implementation)
}

func startTimer() {
	// first run immediately
	onTimer()

	// then every minute
	for range time.Tick(60 * time.Second) {
		onTimer()
	}

}

// tasks that run every minute
func onTimer() {
	// Start Telegram bot if not already running
	go telegramStart()

	// Back up to Telegram if Liquid balance changed
	go liquidBackup(false)

	// Check if pegin can be claimed
	go checkPegin()

	// pre-cache routing statistics
	go ln.FetchForwardingStats()

	// check for updates
	t := internet.GetLatestTag()
	if t != "" {
		latestVersion = t
	}

	// refresh fee rate
	r := internet.GetFeeRate()
	if r > 0 {
		mempoolFeeRate = r
	}
}

func liquidBackup(force bool) {
	// skip backup if missing RPC or Telegram credentials
	if config.Config.ElementsPass == "" || config.Config.ElementsUser == "" || chatId == 0 {
		return
	}

	client, cleanup, err := ps.GetClient(config.Config.RpcHost)
	if err != nil {
		return
	}
	defer cleanup()
	res, err := ps.ListActiveSwaps(client)
	if err != nil {
		return
	}

	// do not backup while a swap is pending
	if len(res.GetSwaps()) > 0 && !force {
		return
	}

	res2, err := ps.LiquidGetBalance(client)
	if err != nil {
		return
	}

	satAmount := res2.GetSatAmount()

	// do not backup if the sat amount did not change
	if satAmount == config.Config.ElementsBackupAmount && !force {
		return
	}

	wallet := config.Config.ElementsWallet
	destinationZip, err := liquid.BackupAndZip(wallet)
	if err != nil {
		log.Println("Error zipping backup:", err)
		return
	}

	err = telegramSendFile(config.Config.DataDir, destinationZip, formatWithThousandSeparators(satAmount))
	if err != nil {
		log.Println("Error sending zip:", err)
		return
	}

	// Delete zip archive
	err = os.Remove(filepath.Join(config.Config.DataDir, destinationZip))
	if err != nil {
		log.Println("Error deleting zip file:", err)
	}

	// save the wallet amount
	config.Config.ElementsBackupAmount = satAmount
	config.Save()
}

func bitcoinHandler(w http.ResponseWriter, r *http.Request) {
	//check for error message to display
	message := ""
	keys, ok := r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		message = keys[0]
	}

	var utxos []ln.UTXO
	cl, clean, er := ln.GetClient()
	if er != nil {
		redirectWithError(w, r, "/config?", er)
		return
	}
	defer clean()

	type Page struct {
		Message          string
		ColorScheme      string
		BitcoinBalance   uint64
		Outputs          *[]ln.UTXO
		PeginTxId        string
		PeginAmount      uint64
		BitcoinApi       string
		Confirmations    int32
		Progress         int32
		Duration         string
		FeeRate          uint32
		MempoolFeeRate   float64
		SuggestedFeeRate uint32
		MinBumpFeeRate   uint32
		CanBump          bool
		CanRBF           bool
		IsCLN            bool
	}

	btcBalance := ln.ConfirmedWalletBalance(cl)
	fee := uint32(mempoolFeeRate)
	confs := int32(0)
	minConfs := int32(1)
	canBump := false
	canCPFP := false

	if config.Config.PeginTxId != "" {
		confs, canCPFP = ln.GetTxConfirmations(cl, config.Config.PeginTxId)
		if confs == 0 {
			canBump = true
			if !ln.CanRBF() {
				// can bump only if there is a change output
				canBump = canCPFP
				if fee > 0 {
					// for CPFP the fee must be 1.5x the market
					fee = fee + fee/2
				}
			}
			if fee < config.Config.PeginFeeRate+1 {
				fee = config.Config.PeginFeeRate + 1 // min increment
			}
		}
	}

	duration := time.Duration(10*(102-confs)) * time.Minute
	formattedDuration := time.Time{}.Add(duration).Format("15h 04m")
	ln.ListUnspent(cl, &utxos, minConfs)

	data := Page{
		Message:          message,
		ColorScheme:      config.Config.ColorScheme,
		BitcoinBalance:   uint64(btcBalance),
		Outputs:          &utxos,
		PeginTxId:        config.Config.PeginTxId,
		PeginAmount:      uint64(config.Config.PeginAmount),
		BitcoinApi:       config.Config.BitcoinApi,
		Confirmations:    confs,
		Progress:         int32(confs * 100 / 102),
		Duration:         formattedDuration,
		FeeRate:          config.Config.PeginFeeRate,
		MempoolFeeRate:   mempoolFeeRate,
		SuggestedFeeRate: fee,
		MinBumpFeeRate:   config.Config.PeginFeeRate + 1,
		CanBump:          canBump,
		CanRBF:           ln.CanRBF(),
		IsCLN:            ln.Implementation == "CLN",
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

		selectedOutputs := r.Form["selected_outputs[]"]
		subtractFeeFromAmount := r.FormValue("subtractfee") == "on"

		totalAmount := int64(0)

		if len(selectedOutputs) > 0 {
			// check that outputs add up

			cl, clean, er := ln.GetClient()
			if er != nil {
				redirectWithError(w, r, "/config?", er)
				return
			}
			defer clean()

			var utxos []ln.UTXO
			ln.ListUnspent(cl, &utxos, int32(1))

			for _, utxo := range utxos {
				for _, output := range selectedOutputs {
					vin := utxo.TxidStr + ":" + strconv.FormatUint(uint64(utxo.OutputIndex), 10)
					if vin == output {
						totalAmount += utxo.AmountSat
					}
				}
			}

			if amount > totalAmount {
				redirectWithError(w, r, "/bitcoin?", errors.New("amount cannot exceed the sum of the selected outputs"))
				return
			}
		}

		if subtractFeeFromAmount {
			if amount != totalAmount {
				redirectWithError(w, r, "/bitcoin?", errors.New("amount should add up to the sum of the selected outputs for 'substract fee' option to be used"))
				return
			}
		}

		if !subtractFeeFromAmount && amount == totalAmount {
			redirectWithError(w, r, "/bitcoin?", errors.New("'subtract fee' option should be used when amount adds up to the selected outputs"))
			return
		}

		// test on pre-existing tx that bitcon core can complete the peg
		tx := "b61ec844027ce18fd3eb91fa7bed8abaa6809c4d3f6cf4952b8ebaa7cd46583a"
		if config.Config.Chain == "testnet" {
			tx = "2c7ec5043fe8ee3cb4ce623212c0e52087d3151c9e882a04073cce1688d6fc1e"
		}

		_, err = bitcoin.GetTxOutProof(tx)
		if err != nil {
			// automatic fallback to getblock.io
			config.Config.BitcoinHost = config.GetBlockIoHost()
			config.Config.BitcoinUser = ""
			config.Config.BitcoinPass = ""
			_, err = bitcoin.GetTxOutProof(tx)
			if err != nil {
				redirectWithError(w, r, "/bitcoin?", errors.New("GetTxOutProof failed, check BitcoinHost in Config"))
				return
			} else {
				// use getblock.io endpoint going forward
				log.Println("Switching to getblock.io bitcoin host endpoint")
				config.Save()
			}
		}

		var addr liquid.PeginAddress

		err = liquid.GetPeginAddress(&addr)
		if err != nil {
			redirectWithError(w, r, "/bitcoin?", err)
			return
		}

		res, err := ln.SendCoinsWithUtxos(&selectedOutputs, addr.MainChainAddress, amount, fee, subtractFeeFromAmount)
		if err != nil {
			redirectWithError(w, r, "/bitcoin?", err)
			return
		}

		// to speed things up, also broadcast it to mempool.space
		internet.SendRawTransaction(res.RawHex)

		log.Println("Peg-in TxId:", res.TxId, "RawHex:", res.RawHex, "Claim script:", addr.ClaimScript)
		duration := time.Duration(1020) * time.Minute
		formattedDuration := time.Time{}.Add(duration).Format("15h 04m")

		telegramSendMessage("⏰ Started peg-in " + formatWithThousandSeparators(uint64(res.AmountSat)) + " sats. Time left: " + formattedDuration)

		config.Config.PeginClaimScript = addr.ClaimScript
		config.Config.PeginAddress = addr.MainChainAddress
		config.Config.PeginAmount = res.AmountSat
		config.Config.PeginTxId = res.TxId
		config.Config.PeginReplacedTxId = ""
		config.Config.PeginFeeRate = uint32(fee)
		config.Save()

		// Redirect to bitcoin page to follow the pegin progress
		http.Redirect(w, r, "/bitcoin", http.StatusSeeOther)
	} else {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func bumpfeeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// Parse the form data
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Error parsing form data", http.StatusBadRequest)
			return
		}

		fee, err := strconv.ParseUint(r.FormValue("feeRate"), 10, 64)
		if err != nil {
			redirectWithError(w, r, "/bitcoin?", err)
			return
		}

		if config.Config.PeginTxId == "" {
			redirectWithError(w, r, "/bitcoin?", errors.New("no pending peg-in"))
			return
		}

		cl, clean, er := ln.GetClient()
		if er != nil {
			redirectWithError(w, r, "/config?", er)
			return
		}
		defer clean()

		confs, _ := ln.GetTxConfirmations(cl, config.Config.PeginTxId)
		if confs > 0 {
			// transaction has been confirmed already
			http.Redirect(w, r, "/bitcoin", http.StatusSeeOther)
			return
		}

		res, err := ln.BumpPeginFee(fee)
		if err != nil {
			redirectWithError(w, r, "/bitcoin?", err)
			return
		}

		// to speed things up, also broadcast it to mempool.space
		internet.SendRawTransaction(res.RawHex)

		if ln.CanRBF() {
			log.Println("RBF TxId:", res.TxId)
			config.Config.PeginReplacedTxId = config.Config.PeginTxId
			config.Config.PeginAmount = res.AmountSat
			config.Config.PeginTxId = res.TxId
		} else {
			log.Println("CPFP successful")
		}

		// save the new rate, so the next bump cannot be lower
		config.Config.PeginFeeRate = uint32(fee)
		config.Save()

		// Redirect to bitcoin page to follow the pegin progress
		http.Redirect(w, r, "/bitcoin", http.StatusSeeOther)
	} else {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func setLogging() (func(), error) {
	// Set log file name
	logFileName := filepath.Join(config.Config.DataDir, "psweb.log")
	var err error
	// Open log file in append mode, create if it doesn't exist
	logFile, err = os.OpenFile(logFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}

	// Set log output to both file and standard output
	multi := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multi)

	log.SetFlags(log.Ldate | log.Ltime)
	if os.Getenv("DEBUG") == "1" {
		log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	}

	cleanup := func() {
		if logFile != nil {
			if err := logFile.Close(); err != nil {
				log.Println("Error closing log file:", err)
			}
		}
	}

	return cleanup, nil
}

func findPeerById(peers []*peerswaprpc.PeerSwapPeer, targetId string) *peerswaprpc.PeerSwapPeer {
	for _, p := range peers {
		if p.NodeId == targetId {
			return p
		}
	}
	return nil // Return nil if peer with given ID is not found
}

// converts a list of peers into an HTML table to display
func convertPeersToHTMLTable(peers []*peerswaprpc.PeerSwapPeer, allowlistedPeers []string, suspiciousPeers []string, swaps []*peerswaprpc.PrettyPrintSwap) string {

	type Table struct {
		AvgLocal int
		HtmlBlob string
	}

	// find last swap timestamps per channel
	swapTimestamps := make(map[uint64]int64)

	for _, swap := range swaps {
		if swapTimestamps[swap.LndChanId] < swap.CreatedAt {
			swapTimestamps[swap.LndChanId] = swap.CreatedAt
		}
	}

	var unsortedTable []Table

	for _, peer := range peers {
		var totalLocal float64
		var totalCapacity float64
		var totalOutflows uint64
		var totalInflows uint64
		var totalFees uint64
		var totalAssistedFees uint64

		channelsTable := "<table style=\"table-layout: fixed; width: 100%; margin-bottom: 0.5em;\">"

		// Construct channels data
		for _, channel := range peer.Channels {
			// red background for inactive channels
			bc := "#590202"
			if config.Config.ColorScheme == "light" {
				bc = "#fcb6b6"
			}

			if channel.Active {
				// green background for active channels
				bc = "#224725"
				if config.Config.ColorScheme == "light" {
					bc = "#e6ffe8"
				}
			}

			channelsTable += "<tr style=\"background-color: " + bc + "\"; >"
			channelsTable += "<td title=\"Local balance\" id=\"scramble\" style=\"width: 10ch; text-align: center\">"
			channelsTable += toMil(channel.LocalBalance)
			channelsTable += "</td><td style=\"text-align: center; vertical-align: middle;\">"
			channelsTable += "<a href=\"/peer?id=" + peer.NodeId + "\">"

			local := float64(channel.LocalBalance)
			capacity := float64(channel.LocalBalance + channel.RemoteBalance)
			totalLocal += local
			totalCapacity += capacity
			tooltip := " in the last 6 months"

			// timestamp of the last swap or 6m horizon
			lastSwapTimestamp := time.Now().AddDate(0, -6, 0).Unix()
			if swapTimestamps[channel.ChannelId] > lastSwapTimestamp {
				lastSwapTimestamp = swapTimestamps[channel.ChannelId]
				tooltip = " since the last swap " + timePassedAgo(time.Unix(lastSwapTimestamp, 0).UTC())
			}

			stats := ln.GetNetFlow(channel.ChannelId, uint64(lastSwapTimestamp))
			totalFees += stats.FeeSat
			totalAssistedFees += stats.AssistedFeeSat
			totalOutflows += stats.AmountOut
			totalInflows += stats.AmountIn

			netFlow := float64(stats.AmountOut - stats.AmountIn)

			bluePct := int(local * 100 / capacity)
			greenPct := int(0)
			redPct := int(0)
			previousBlue := bluePct
			previousRed := redPct

			if netFlow == 0 {
				tooltip = "No flow" + tooltip
			} else {
				if netFlow > 0 {
					greenPct = int(local * 100 / capacity)
					bluePct = int((local - netFlow) * 100 / capacity)
					previousBlue = greenPct
					tooltip = "Net inflow " + toMil(uint64(netFlow)) + tooltip
				}

				if netFlow < 0 {
					bluePct = int(local * 100 / capacity)
					redPct = int((local - netFlow) * 100 / capacity)
					previousRed = bluePct
					tooltip = "Net outflow " + toMil(uint64(-netFlow)) + tooltip
				}
			}

			currentProgress := fmt.Sprintf("%d%% 100%%, %d%% 100%%, %d%% 100%%, 100%% 100%%", bluePct, redPct, greenPct)
			previousProgress := fmt.Sprintf("%d%% 100%%, %d%% 100%%, %d%% 100%%, 100%% 100%%", previousBlue, previousRed, greenPct)

			channelsTable += "<div title=\"" + tooltip + "\" class=\"progress\" style=\"background-size: " + currentProgress + ";\" onmouseover=\"this.style.backgroundSize = '" + previousProgress + "';\" onmouseout=\"this.style.backgroundSize = '" + currentProgress + "';\"></div>"
			channelsTable += "</a></td>"
			channelsTable += "<td title=\"Remote balance\" id=\"scramble\" style=\"width: 10ch; text-align: center\">"
			channelsTable += toMil(channel.RemoteBalance)
			channelsTable += "</td></tr>"
		}
		channelsTable += "</table>"

		// count total outbound to sort peers later
		pct := int(1000000 * totalLocal / totalCapacity)

		peerTable := "<table style=\"table-layout:fixed; width: 100%\">"
		peerTable += "<tr style=\"border: 1px dotted\">"
		peerTable += "<td  class=\"truncate\" id=\"scramble\" style=\"float: left; text-align: left; width: 70%;\">"

		// alias is a link to open peer details page
		peerTable += "<a href=\"/peer?id=" + peer.NodeId + "\">"

		if stringIsInSlice(peer.NodeId, allowlistedPeers) {
			peerTable += "<span title=\"Peer is whitelisted\">✅&nbsp</span>"
		} else {
			peerTable += "<span title=\"Peer is blacklisted\">⛔&nbsp</span>"
		}

		if stringIsInSlice(peer.NodeId, suspiciousPeers) {
			peerTable += "<span title=\"Peer is marked suspicious\">🕵&nbsp</span>"
		}

		peerTable += "<span title=\"Click for peer details\">" + getNodeAlias(peer.NodeId)
		peerTable += "</span></a>"

		peerTable += "</td><td style=\"float: center; text-align: center; width:15ch;\">"

		ppm := uint64(0)
		if totalOutflows > 0 {
			ppm = totalFees * 100_000_000 / totalOutflows
		}
		peerTable += "<span title=\"Total outbound fees since the last swap or 6m. PPM: " + formatWithThousandSeparators(ppm) + "\">" + formatWithThousandSeparators(totalFees) + "</span> / "

		ppm = 0
		if totalInflows > 0 {
			ppm = totalAssistedFees * 100_000_000 / totalInflows
		}
		peerTable += "<span title=\"Total assisted fees since the last swap or 6m. PPM: " + formatWithThousandSeparators(ppm) + "\">" + formatWithThousandSeparators(totalAssistedFees) + "</span>"

		peerTable += "</td><td style=\"float: right; text-align: right; width:8ch;\">"

		if stringIsInSlice("lbtc", peer.SupportedAssets) {
			peerTable += "<span title=\"L-BTC swaps enabled\"> 🌊&nbsp</span>"
		}
		if stringIsInSlice("btc", peer.SupportedAssets) {
			peerTable += "<span title=\"BTC swaps enabled\" style=\"color: #FF9900; font-weight: bold;\">₿</span>&nbsp"
		}
		if peer.SwapsAllowed {
			peerTable += "<span title=\"Peer whilelisted us\">✅</span>"
		} else {
			peerTable += "<span title=\"Peer did not whitelist us\">⛔</span>"
		}
		peerTable += "</td></tr></table>"

		unsortedTable = append(unsortedTable, Table{
			AvgLocal: pct,
			HtmlBlob: peerTable + channelsTable,
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
// if nodeId != "" then only show swaps for that node Id
func convertSwapsToHTMLTable(swaps []*peerswaprpc.PrettyPrintSwap, nodeId string, swapState string, swapRole string) string {

	if len(swaps) == 0 {
		return ""
	}

	type Table struct {
		TimeStamp int64
		HtmlBlob  string
	}
	var unsortedTable []Table

	for _, swap := range swaps {
		// filter by node Id
		if nodeId != "" && nodeId != swap.PeerNodeId {
			continue
		}

		// filter by simple swap state
		if swapState != "" && swapState != simplifySwapState(swap.State) {
			continue
		}

		// filter by simple swap state
		if swapRole != "" && swapRole != swap.Role {
			continue
		}

		table := "<tr>"
		table += "<td style=\"width: 30%; text-align: left\">"

		tm := timePassedAgo(time.Unix(swap.CreatedAt, 0).UTC())

		// clicking on timestamp will open swap details page
		table += "<a title=\"Open swap details page\" href=\"/swap?id=" + swap.Id + "\">" + tm + "</a> "
		table += "</td><td style=\"text-align: left\">"

		// clicking on swap status will filter swaps with equal status
		table += "<a title=\"Filter by state: " + simplifySwapState(swap.State) + "\" href=\"/?id=" + nodeId + "&state=" + simplifySwapState(swap.State) + "&role=" + swapRole + "\">"
		table += visualiseSwapState(swap.State, false) + "&nbsp</a>"
		table += formatWithThousandSeparators(swap.Amount)

		asset := "🌊"
		if swap.Asset == "btc" {
			asset = "<span style=\"color: #FF9900; font-weight: bold;\">₿</span>"
		}

		switch swap.Type + swap.Role {
		case "swap-outsender":
			table += " ⚡&nbsp⇨&nbsp" + asset
		case "swap-insender":
			table += " " + asset + "&nbsp⇨&nbsp⚡"
		case "swap-outreceiver":
			table += " " + asset + "&nbsp⇨&nbsp⚡"
		case "swap-inreceiver":
			table += " ⚡&nbsp⇨&nbsp" + asset
		}

		table += "</td><td id=\"scramble\" style=\"overflow-wrap: break-word;\">"

		role := ""
		switch swap.Role {
		case "receiver":
			role = "⇦"
		case "sender":
			role = "⇨"
		}

		// clicking on role will filter this direction only
		table += "<a title=\"Filter by role: " + swap.Role + "\" href=\"/?&id=" + nodeId + "&state=" + swapState + "&role=" + swap.Role + "\">"
		table += " " + role + "&nbsp<a>"

		// clicking on node alias will filter its swaps only
		table += "<a title=\"Filter swaps by this peer\" href=\"/?id=" + swap.PeerNodeId + "&state=" + swapState + "&role=" + swapRole + "\">"
		table += getNodeAlias(swap.PeerNodeId)
		table += "</a>"
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

	var counter uint
	table := "<table style=\"table-layout:fixed; width: 100%\">"
	for _, t := range unsortedTable {
		counter++
		if counter > config.Config.MaxHistory {
			break
		}
		table += t.HtmlBlob
	}
	table += "</table>"
	return table
}

// Check Peg-in status
func checkPegin() {
	if config.Config.PeginTxId == "" {
		return
	}

	cl, clean, er := ln.GetClient()
	if er != nil {
		return
	}
	defer clean()

	confs, _ := ln.GetTxConfirmations(cl, config.Config.PeginTxId)
	if confs < 0 && config.Config.PeginReplacedTxId != "" {
		confs, _ = ln.GetTxConfirmations(cl, config.Config.PeginReplacedTxId)
		if confs > 0 {
			// RBF replacement conflict: the old transaction mined before the new one
			config.Config.PeginTxId = config.Config.PeginReplacedTxId
			config.Config.PeginReplacedTxId = ""
			log.Println("The last RBF failed as previous tx mined earlier, switching to prior txid for the peg-in progress:", config.Config.PeginTxId)
		}
	}

	if confs >= 102 {
		// claim pegin
		failed := false
		proof := ""
		txid := ""
		rawTx, err := ln.GetRawTransaction(cl, config.Config.PeginTxId)
		if err == nil {
			proof, err = bitcoin.GetTxOutProof(config.Config.PeginTxId)
			if err == nil {
				txid, err = liquid.ClaimPegin(rawTx, proof, config.Config.PeginClaimScript)
				// claimpegin takes long time, allow it to timeout
				if err != nil && err.Error() != "timeout reading data from server" {
					failed = true
				}
			} else {
				failed = true
			}
		} else {
			failed = true
		}

		if failed {
			log.Println("Peg-in claim FAILED!")
			log.Println("Mainchain TxId:", config.Config.PeginTxId)
			log.Println("Raw tx:", rawTx)
			log.Println("Proof:", proof)
			log.Println("Claim Script:", config.Config.PeginClaimScript)
			telegramSendMessage("❗ Peg-in claim FAILED! See log for details.")
		} else {
			log.Println("Peg-in success! Liquid TxId:", txid)
			telegramSendMessage("💸 Peg-in success!")
		}

		// stop trying after one attempt
		config.Config.PeginTxId = ""
		config.Save()
	}
}

func getNodeAlias(key string) string {
	// search in cache
	for _, n := range aliasCache {
		if n.PublicKey == key {
			return n.Alias
		}
	}

	// try lightning
	alias := ln.GetAlias(key)

	if alias == "" {
		// try mempool
		alias = internet.GetNodeAlias(key)
	}

	if alias == "" {
		// return first 20 chars of key
		return key[:20]
	}

	// save to cache if alias was found
	aliasCache = append(aliasCache, AliasCache{
		PublicKey: key,
		Alias:     alias,
	})

	return alias
}

// preemptively load Aliases in cache
func cacheAliases() {
	client, cleanup, err := ps.GetClient(config.Config.RpcHost)
	if err != nil {
		return
	}
	defer cleanup()

	res, err := ps.ListPeers(client)
	if err != nil {
		return
	}

	peers := res.GetPeers()
	for _, peer := range peers {
		getNodeAlias(peer.NodeId)
	}
}
