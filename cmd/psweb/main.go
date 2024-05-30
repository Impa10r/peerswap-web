package main

import (
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
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

const (
	// App version tag
	version = "v1.5.1"
)

type SwapParams struct {
	PeerAlias string
	ChannelId uint64
	Amount    uint64
	PPM       uint64
}

var (
	aliasCache = make(map[string]string)
	templates  = template.New("")
	//go:embed static/*
	staticFiles embed.FS
	//go:embed templates/*.gohtml
	tplFolder     embed.FS
	logFile       *os.File
	latestVersion = version
	// Bitcoin sat/vB from mempool.space
	mempoolFeeRate = float64(0)
	// onchain realized transaction costs
	txFee = make(map[string]int64)
)

func main() {

	var (
		dataDir     = flag.String("datadir", "", "Path to config folder (default: ~/.peerswap)")
		showHelp    = flag.Bool("help", false, "Show help")
		showVersion = flag.Bool("version", false, "Show version")
	)

	flag.Parse()

	if *showHelp {
		fmt.Println("A lightweight server-side rendered Web UI for PeerSwap, which allows trustless p2p submarine swaps Lightning<->BTC and Lightning<->Liquid. Also facilitates BTC->Liquid peg-ins. PeerSwap with Liquid is a great cost efficient way to rebalance lightning channels.")
		fmt.Println("Usage:")
		flag.PrintDefaults()
		os.Exit(0)
	}

	if *showVersion {
		fmt.Println("Version:", version, "for", ln.Implementation)
		os.Exit(0)
	}

	// loading from config file or creating default one
	config.Load(*dataDir)

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
			"fs":   formatSigned,
			"m":    toMil,
		}).
		ParseFS(tplFolder, templateNames...))

	// create an embedded Filesystem
	var staticFS = http.FS(staticFiles)
	fs := http.FileServer(staticFS)

	r := mux.NewRouter()

	// Serve static files
	r.PathPrefix("/static/").Handler(fs)

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
	r.HandleFunc("/ca", caHandler)

	if config.Config.SecureConnection {
		// HTTP redirection
		go func() {
			httpMux := http.NewServeMux()
			httpMux.HandleFunc("/", redirectToHTTPS)
			log.Println("Listening on http://localhost:" + config.Config.ListenPort + " for redirection...")
			if err := http.ListenAndServe(":"+config.Config.ListenPort, httpMux); err != nil {
				log.Fatalf("Failed to start HTTP server: %v\n", err)
			}
		}()

		go serveHTTPS(retryMiddleware(r))
		log.Println("Listening HTTPS on port " + config.Config.SecurePort)
	} else {
		// Start HTTP server
		http.Handle("/", retryMiddleware(r))
		go func() {
			if err := http.ListenAndServe(":"+config.Config.ListenPort, nil); err != nil {
				log.Fatal(err)
			}
		}()
		log.Println("Listening on http://localhost:" + config.Config.ListenPort)
	}

	// Start timer to run every minute
	go startTimer()

	// to speed up first load of home page
	go cacheAliases()

	// CLN: cache forwarding stats
	go ln.CacheForwards()

	// fetch all chain costs (synchronous to protect map writes)
	cacheSwapCosts()

	// Handle termination signals
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for termination signal
	sig := <-signalChan
	log.Printf("Received termination signal: %s\n", sig)

	// Exit the program gracefully
	os.Exit(0)
}

func redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	url := "https://" + strings.Split(r.Host, ":")[0] + ":" + config.Config.SecurePort + r.URL.String()
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func serveHTTPS(handler http.Handler) {
	// Load your certificate and private key
	certFile := filepath.Join(config.Config.DataDir, "server.crt")
	keyFile := filepath.Join(config.Config.DataDir, "server.key")

	//regenerate from CA if deleted
	if !fileExists(certFile) {
		config.GenereateServerCertificate()
	}

	// Load your server certificate and private key
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Fatalf("Failed to load server certificate: %v", err)
	}

	// Load CA certificate
	caCert, err := os.ReadFile(filepath.Join(config.Config.DataDir, "CA.crt"))
	if err != nil {
		log.Fatalf("Failed to load CA certificate: %v", err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	// Configure TLS settings
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    caCertPool,
		ClientAuth:   tls.RequireAndVerifyClientCert, // Require and verify client certificate
		MinVersion:   tls.VersionTLS12,               // Force TLS 1.2 or higher
	}

	server := &http.Server{
		Addr:      ":" + config.Config.SecurePort,
		Handler:   handler,
		TLSConfig: tlsConfig,
		// Assign the mute logger to prevent log spamming
		ErrorLog: NewMuteLogger(),
	}

	// Start the HTTPS server
	err = server.ListenAndServeTLS(certFile, keyFile)
	if err != nil {
		log.Fatalf("Failed to start HTTPS server: %v\n", err)
	}
}

func indexHandler(w http.ResponseWriter, r *http.Request) {

	if config.Config.ElementsPass == "" || config.Config.ElementsUser == "" {
		http.Redirect(w, r, "/config?err=welcome", http.StatusSeeOther)
		return
	}

	// PeerSwap RPC client
	// this method will fail if peerswap is not running or misconfigured
	client, cleanup, err := ps.GetClient(config.Config.RpcHost)
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}
	defer cleanup()

	res, err := ps.ListSwaps(client)
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}
	swaps := res.GetSwaps()

	res2, err := ps.LiquidGetBalance(client)
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}

	satAmount := res2.GetSatAmount()

	// Lightning RPC client
	cl, clean, er := ln.GetClient()
	if er != nil {
		redirectWithError(w, r, "/config?", er)
		return
	}
	defer clean()

	btcBalance := ln.ConfirmedWalletBalance(cl)

	//check for error message to display
	errorMessage := ""
	keys, ok := r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		errorMessage = keys[0]
	}

	//check for pop-up message to display
	popupMessage := ""
	keys, ok = r.URL.Query()["msg"]
	if ok && len(keys[0]) > 0 {
		popupMessage = keys[0]
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

	var peers []*peerswaprpc.PeerSwapPeer

	res3, err := ps.ReloadPolicyFile(client)
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}

	allowlistedPeers := res3.GetAllowlistedPeers()
	suspiciousPeers := res3.GetSuspiciousPeerList()

	res4, err := ps.ListPeers(client)
	if err != nil {
		redirectWithError(w, r, "/config?", err)
		return
	}
	peers = res4.GetPeers()

	// refresh forwarding stats
	ln.CacheForwards()

	// get fee rates for all channels
	outboundFeeRates := make(map[uint64]int64)
	inboundFeeRates := make(map[uint64]int64)

	ln.FeeReport(cl, outboundFeeRates, inboundFeeRates)

	_, showAll := r.URL.Query()["showall"]

	peerTable := convertPeersToHTMLTable(peers, allowlistedPeers, suspiciousPeers, swaps, outboundFeeRates, inboundFeeRates, showAll)

	//check whether to display non-PS channels or swaps
	listSwaps := ""
	nonPeerTable := ""

	if showAll {
		// make a list of peerswap peers
		var psIds []string

		for _, peer := range peers {
			psIds = append(psIds, peer.NodeId)
		}

		// Get the remaining Lightning peers
		res5, err := ln.ListPeers(cl, "", &psIds)
		if err != nil {
			redirectWithError(w, r, "/config?", err)
			return
		}
		otherPeers := res5.GetPeers()
		nonPeerTable = convertOtherPeersToHTMLTable(otherPeers, outboundFeeRates, inboundFeeRates, showAll)

		if nonPeerTable == "" && popupMessage == "" {
			popupMessage = "🥳 Congratulations, all your peers use PeerSwap!"
			listSwaps = convertSwapsToHTMLTable(swaps, nodeId, state, role)
		}
	} else {
		listSwaps = convertSwapsToHTMLTable(swaps, nodeId, state, role)
	}

	type Page struct {
		AllowSwapRequests bool
		BitcoinSwaps      bool
		ErrorMessage      string
		PopUpMessage      string
		ColorScheme       string
		LiquidBalance     uint64
		ListPeers         string
		OtherPeers        string
		ListSwaps         string
		BitcoinBalance    uint64
		Filter            bool
		MempoolFeeRate    float64
		AutoSwapEnabled   bool
		PeginPending      bool
	}

	data := Page{
		AllowSwapRequests: config.Config.AllowSwapRequests,
		BitcoinSwaps:      config.Config.BitcoinSwaps,
		ErrorMessage:      errorMessage,
		PopUpMessage:      popupMessage,
		MempoolFeeRate:    mempoolFeeRate,
		ColorScheme:       config.Config.ColorScheme,
		LiquidBalance:     satAmount,
		ListPeers:         peerTable,
		OtherPeers:        nonPeerTable,
		ListSwaps:         listSwaps,
		BitcoinBalance:    uint64(btcBalance),
		Filter:            nodeId != "" || state != "" || role != "",
		AutoSwapEnabled:   config.Config.AutoSwapEnabled,
		PeginPending:      config.Config.PeginTxId != "",
	}

	// executing template named "homepage"
	err = templates.ExecuteTemplate(w, "homepage", data)
	if err != nil {
		log.Fatalln(err)
	}
}

func peerHandler(w http.ResponseWriter, r *http.Request) {
	keys, ok := r.URL.Query()["id"]
	if !ok || len(keys[0]) < 1 {
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

	res5, err := ps.ListSwaps(client)
	if err != nil {
		return
	}
	swaps := res5.GetSwaps()

	senderInFee := int64(0)
	receiverInFee := int64(0)
	receiverOutFee := int64(0)

	for _, swap := range swaps {
		switch swap.Type + swap.Role {
		case "swap-insender":
			if swap.PeerNodeId == id {
				senderInFee += swapCost(swap)
			}
		case "swap-outreceiver":
			if swap.InitiatorNodeId == id {
				receiverOutFee += swapCost(swap)
			}
		case "swap-inreceiver":
			if swap.InitiatorNodeId == id {
				receiverInFee += swapCost(swap)
			}
		}
	}

	senderInFeePPM := int64(0)
	receiverInFeePPM := int64(0)
	receiverOutFeePPM := int64(0)
	senderOutFeePPM := int64(0)

	// Get Lightning client
	cl, clean, er := ln.GetClient()
	if er != nil {
		redirectWithError(w, r, "/config?", er)
		return
	}
	defer clean()

	btcBalance := ln.ConfirmedWalletBalance(cl)

	psPeer := true
	if peer == nil {
		// Search amoung all Lighting peers
		res, err := ln.ListPeers(cl, id, nil)
		if err != nil {
			redirectWithError(w, r, "/?", err)
			return
		}
		peer = res.GetPeers()[0]
		psPeer = false
	} else {
		if peer.AsSender.SatsOut > 0 {
			senderOutFeePPM = int64(peer.PaidFee) * 1_000_000 / int64(peer.AsSender.SatsOut)
		}
		if peer.AsSender.SatsIn > 0 {
			senderInFeePPM = senderInFee * 1_000_000 / int64(peer.AsSender.SatsIn)
		}
		if peer.AsReceiver.SatsOut > 0 {
			receiverOutFeePPM = receiverOutFee * 1_000_000 / int64(peer.AsReceiver.SatsOut)
		}
		if peer.AsReceiver.SatsIn > 0 {
			receiverInFeePPM = receiverInFee * 1_000_000 / int64(peer.AsReceiver.SatsIn)
		}
	}

	var sumLocal uint64
	var sumRemote uint64
	var stats []*ln.ForwardingStats
	var channelInfo []*ln.ChanneInfo
	var keysendSats = uint64(1)

	var utxosBTC []ln.UTXO
	ln.ListUnspent(cl, &utxosBTC, 1)

	var utxosLBTC []liquid.UTXO
	liquid.ListUnspent(&utxosLBTC)

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

		// should not be less than both Min HTLC setting
		keysendSats = max(keysendSats, msatToSatUp(info.PeerMinHtlcMsat))
		keysendSats = max(keysendSats, msatToSatUp(info.OurMinHtlcMsat))
	}

	//check for error errorMessage to display
	errorMessage := ""
	keys, ok = r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		errorMessage = keys[0]
	}

	// get routing stats

	type Page struct {
		ErrorMessage      string
		PopUpMessage      string
		MempoolFeeRate    float64
		BtcFeeRate        float64
		ColorScheme       string
		Peer              *peerswaprpc.PeerSwapPeer
		PeerAlias         string
		NodeUrl           string
		Allowed           bool
		Suspicious        bool
		LBTC              bool
		BTC               bool
		LiquidBalance     uint64
		BitcoinBalance    uint64
		ActiveSwaps       string
		DirectionIn       bool
		Stats             []*ln.ForwardingStats
		ChannelInfo       []*ln.ChanneInfo
		PeerSwapPeer      bool
		MyAlias           string
		SenderOutFeePPM   int64
		SenderInFee       int64
		ReceiverInFee     int64
		ReceiverOutFee    int64
		SenderInFeePPM    int64
		ReceiverInFeePPM  int64
		ReceiverOutFeePPM int64
		KeysendSats       uint64
		OutputsBTC        *[]ln.UTXO
		OutputsLBTC       *[]liquid.UTXO
		ReserveLBTC       uint64
		ReserveBTC        uint64
	}

	feeRate := liquid.EstimateFee()
	if !psPeer {
		feeRate = mempoolFeeRate
	}

	// to be conservative
	bitcoinFeeRate := max(ln.EstimateFee(), mempoolFeeRate)

	data := Page{
		ErrorMessage:      errorMessage,
		PopUpMessage:      "",
		BtcFeeRate:        bitcoinFeeRate,
		MempoolFeeRate:    feeRate,
		ColorScheme:       config.Config.ColorScheme,
		Peer:              peer,
		PeerAlias:         getNodeAlias(peer.NodeId),
		NodeUrl:           config.Config.NodeApi,
		Allowed:           stringIsInSlice(peer.NodeId, allowlistedPeers),
		Suspicious:        stringIsInSlice(peer.NodeId, suspiciousPeers),
		BTC:               stringIsInSlice("btc", peer.SupportedAssets),
		LBTC:              stringIsInSlice("lbtc", peer.SupportedAssets),
		LiquidBalance:     satAmount,
		BitcoinBalance:    uint64(btcBalance),
		ActiveSwaps:       convertSwapsToHTMLTable(activeSwaps, "", "", ""),
		DirectionIn:       sumLocal < sumRemote,
		Stats:             stats,
		ChannelInfo:       channelInfo,
		PeerSwapPeer:      psPeer,
		MyAlias:           ln.GetMyAlias(),
		SenderOutFeePPM:   senderOutFeePPM,
		SenderInFee:       senderInFee,
		ReceiverInFee:     receiverInFee,
		ReceiverOutFee:    receiverOutFee,
		SenderInFeePPM:    senderInFeePPM,
		ReceiverInFeePPM:  receiverInFeePPM,
		ReceiverOutFeePPM: receiverOutFeePPM,
		KeysendSats:       keysendSats,
		OutputsBTC:        &utxosBTC,
		OutputsLBTC:       &utxosLBTC,
		ReserveLBTC:       ln.SwapFeeReserveLBTC,
		ReserveBTC:        ln.SwapFeeReserveBTC,
	}

	// executing template named "peer"
	err = templates.ExecuteTemplate(w, "peer", data)
	if err != nil {
		log.Fatalln(err)
	}
}

func swapHandler(w http.ResponseWriter, r *http.Request) {
	keys, ok := r.URL.Query()["id"]
	if !ok || len(keys[0]) < 1 {
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
		ErrorMessage   string
		PopUpMessage   string
		MempoolFeeRate float64
		IsPending      bool
	}

	data := Page{
		ColorScheme:    config.Config.ColorScheme,
		Id:             id,
		ErrorMessage:   "",
		PopUpMessage:   "",
		MempoolFeeRate: mempoolFeeRate,
		IsPending:      isPending,
	}

	// executing template named "swap"
	err = templates.ExecuteTemplate(w, "swap", data)
	if err != nil {
		log.Fatalln(err)
	}
}

// Updates swap page live
func updateHandler(w http.ResponseWriter, r *http.Request) {
	keys, ok := r.URL.Query()["id"]
	if !ok || len(keys[0]) < 1 {
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
				<h4 class="title is-4">`
	swapData += visualiseSwapState(swap.State, true)
	swapData += `</h4>
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
	swapData += `&nbsp<a href="`
	swapData += config.Config.NodeApi + "/" + swap.InitiatorNodeId
	swapData += `" target="_blank">`
	swapData += getNodeAlias(swap.InitiatorNodeId)
	swapData += `</a></td></tr>
			<tr><td style="text-align: right">Peer:</td><td style="overflow-wrap: break-word;">`
	swapData += `&nbsp<a href="`
	swapData += config.Config.NodeApi + "/" + swap.PeerNodeId
	swapData += `" target="_blank">`
	swapData += getNodeAlias(swap.PeerNodeId)
	swapData += `</a></td></tr>
			<tr><td style="text-align: right">Amount:</td><td>`
	swapData += formatWithThousandSeparators(swap.Amount)
	swapData += ` sats</td></tr>
			<tr><td style="text-align: right">ChannelId:</td><td>`
	swapData += swap.ChannelId
	swapData += `</td></tr>`
	if swap.OpeningTxId != "" {
		swapData += `<tr><td style="text-align: right">OpeningTxId:</td><td style="overflow-wrap: break-word;">`
		swapData += `&nbsp<a href="`
		swapData += url + swap.OpeningTxId
		swapData += `" target="_blank">`
		swapData += swap.OpeningTxId
		swapData += `</a>`
	}
	if swap.ClaimTxId != "" {
		swapData += `</td></tr>
			<tr><td style="text-align: right">ClaimTxId:</td><td style="overflow-wrap: break-word;">`
		swapData += `&nbsp<a href="`
		swapData += url + swap.ClaimTxId
		swapData += `" target="_blank">`
		swapData += swap.ClaimTxId
		swapData += `</a></td></tr>`
	}
	if swap.CancelMessage != "" {
		swapData += `<tr><td style="text-align: right">CancelMsg:</td><td>`
		swapData += swap.CancelMessage
		swapData += `</td></tr>`
	}
	swapData += `<tr><td style="text-align: right">LndChanId:</td><td>`
	swapData += strconv.FormatUint(uint64(swap.LndChanId), 10)

	cost := swapCost(swap)
	if cost != 0 {
		ppm := cost * 1_000_000 / int64(swap.Amount)

		swapData += `<tr><td style="text-align: right">Swap Cost:</td><td>`
		swapData += formatSigned(cost) + " sats"
		swapData += `<tr><td style="text-align: right">Cost PPM:</td><td>`
		swapData += formatSigned(ppm)
	}

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
	errorMessage := ""
	keys, ok := r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		errorMessage = keys[0]
	}

	// Get the hostname of the machine
	hostname := config.GetHostname()

	// populate server IP if empty
	if config.Config.ServerIPs == "" {
		ip := strings.Split(r.Host, ":")[0]
		if net.ParseIP(ip) != nil && ip != "127.0.0.1" {
			config.Config.ServerIPs = ip
		}
	}

	type Page struct {
		ErrorMessage   string
		PopUpMessage   string
		MempoolFeeRate float64
		ColorScheme    string
		Config         config.Configuration
		Version        string
		Latest         string
		Implementation string
		HTTPS          string
	}

	data := Page{
		ErrorMessage:   errorMessage,
		PopUpMessage:   "",
		MempoolFeeRate: mempoolFeeRate,
		ColorScheme:    config.Config.ColorScheme,
		Config:         config.Config,
		Version:        version,
		Latest:         latestVersion,
		Implementation: ln.Implementation,
		HTTPS:          "https://" + hostname + ".local:" + config.Config.SecurePort,
	}

	// executing template named "config"
	err := templates.ExecuteTemplate(w, "config", data)
	if err != nil {
		log.Fatalln(err)
	}
}

func caHandler(w http.ResponseWriter, r *http.Request) {
	//check for error message to display
	errorMessage := ""
	keys, ok := r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		errorMessage = keys[0]
	}

	hostname := config.GetHostname()

	urls := []string{
		"https://localhost:" + config.Config.SecurePort,
		"https://" + hostname + ".local:" + config.Config.SecurePort,
	}

	if config.Config.ServerIPs != "" {
		for _, ip := range strings.Split(config.Config.ServerIPs, " ") {
			urls = append(urls, "https://"+ip+":"+config.Config.SecurePort)
		}
	}

	password, err := config.GeneratePassword(10)
	if err != nil {
		log.Println("GeneratePassword:", err)
		redirectWithError(w, r, "/config?", err)
		return
	}

	type Page struct {
		ErrorMessage   string
		PopUpMessage   string
		MempoolFeeRate float64
		ColorScheme    string
		Config         config.Configuration
		URLs           []string
		Password       string
	}

	data := Page{
		ErrorMessage:   errorMessage,
		PopUpMessage:   "",
		MempoolFeeRate: mempoolFeeRate,
		ColorScheme:    config.Config.ColorScheme,
		Config:         config.Config,
		URLs:           urls,
		Password:       password,
	}

	if !fileExists(filepath.Join(config.Config.DataDir, "CA.crt")) {
		err := config.GenerateCA()
		if err != nil {
			log.Println("Error generating CA.crt:", err)
			redirectWithError(w, r, "/config?", err)
			return
		}
	}

	err = config.GenerateClientCertificate(password)
	if err != nil {
		log.Println("Error generating client.p12:", err)
		redirectWithError(w, r, "/config?", err)
		return
	}

	// executing template named "ca"
	err = templates.ExecuteTemplate(w, "ca", data)
	if err != nil {
		log.Fatalln(err)
	}
}

func liquidHandler(w http.ResponseWriter, r *http.Request) {
	//check for error message to display
	errorMessage := ""
	keys, ok := r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		errorMessage = keys[0]
	}

	//check for pop-up message to display
	popupMessage := ""
	keys, ok = r.URL.Query()["msg"]
	if ok && len(keys[0]) > 0 {
		popupMessage = keys[0]
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

	satAmount := res2.GetSatAmount()

	var candidate SwapParams

	if err := findSwapInCandidate(&candidate); err != nil {
		log.Printf("unable findSwapInCandidate: %v", err)
		redirectWithError(w, r, "/liquid?", err)
		return
	}

	type Page struct {
		ErrorMessage            string
		PopUpMessage            string
		MempoolFeeRate          float64
		ColorScheme             string
		LiquidAddress           string
		LiquidBalance           uint64
		TxId                    string
		LiquidUrl               string
		LiquidApi               string
		AutoSwapEnabled         bool
		AutoSwapThresholdAmount uint64
		AutoSwapThresholdPPM    uint64
		AutoSwapCandidate       *SwapParams
		AutoSwapTargetPct       uint64
	}

	data := Page{
		ErrorMessage:            errorMessage,
		PopUpMessage:            popupMessage,
		MempoolFeeRate:          liquid.EstimateFee(),
		ColorScheme:             config.Config.ColorScheme,
		LiquidAddress:           addr,
		LiquidBalance:           satAmount,
		TxId:                    txid,
		LiquidUrl:               config.Config.LiquidApi + "/tx/" + txid,
		LiquidApi:               config.Config.LiquidApi,
		AutoSwapEnabled:         config.Config.AutoSwapEnabled,
		AutoSwapThresholdAmount: config.Config.AutoSwapThresholdAmount,
		AutoSwapThresholdPPM:    config.Config.AutoSwapThresholdPPM,
		AutoSwapTargetPct:       config.Config.AutoSwapTargetPct,
		AutoSwapCandidate:       &candidate,
	}

	// executing template named "liquid"
	err = templates.ExecuteTemplate(w, "liquid", data)
	if err != nil {
		log.Fatalln(err)
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

		client, cleanup, err := ps.GetClient(config.Config.RpcHost)
		if err != nil {
			redirectWithError(w, r, "/config?", err)
			return
		}
		defer cleanup()

		switch action {
		case "setFee":
			showAll := ""
			if r.FormValue("showAll") == "true" {
				// reloaded index page to show all peers
				showAll = "showall&"
			}

			feeRate, err := strconv.ParseInt(r.FormValue("feeRate"), 10, 64)
			if err != nil {
				redirectWithError(w, r, "/?"+showAll, err)
				return
			}

			channelId, err := strconv.ParseUint(r.FormValue("channelId"), 10, 64)
			if err != nil {
				redirectWithError(w, r, "/?"+showAll, err)
				return
			}

			inbound := r.FormValue("direction") == "inbound"

			if inbound {
				if ln.Implementation == "CLN" || !ln.CanRBF() {
					// CLN and LND < 0.18 cannot set inbound fees
					redirectWithError(w, r, "/?"+showAll, errors.New("Inbound fees are not enabled by your LN backend"))
					return
				}

				if feeRate > 0 {
					// Only discounts are allowed for now
					redirectWithError(w, r, "/?"+showAll, errors.New("Inbound fee rate cannot be positive"))
					return
				}
			} else {
				if feeRate < 0 {
					// Only discounts are allowed for now
					redirectWithError(w, r, "/?"+showAll, errors.New("Outbound fee rate cannot be negative"))
					return
				}
			}

			err = ln.SetFeeRate(r.FormValue("peerNodeId"), channelId, feeRate, inbound)
			if err != nil {
				redirectWithError(w, r, "/?"+showAll, err)
				return
			}

			// all good, display confirmation
			msg := strings.Title(r.FormValue("direction")) + " fee rate updated to " + formatSigned(feeRate)
			http.Redirect(w, r, "/?"+showAll+"msg="+msg, http.StatusSeeOther)

		case "enableHTTPS":
			// restart with HTTPS listener
			if err := config.GenereateServerCertificate(); err == nil {
				config.Config.SecureConnection = true
				config.Save()
				restart(w, r)
			} else {
				redirectWithError(w, r, "/ca?", err)
				return
			}
		case "keySend":
			dest := r.FormValue("nodeId")
			message := r.FormValue("keysendMessage")

			amount, err := strconv.ParseInt(r.FormValue("keysendAmount"), 10, 64)
			if err != nil {
				redirectWithError(w, r, "/peer?id="+dest+"&", err)
				return
			}

			err = ln.SendKeysendMessage(dest, amount, message)
			if err != nil {
				redirectWithError(w, r, "/peer?id="+dest+"&", err)
				return
			}

			msg := "Keysend invitation sent to " + getNodeAlias(dest)

			log.Println(msg)

			// Load main page with all pees and a pop-up message
			http.Redirect(w, r, "/?showall&msg="+msg, http.StatusSeeOther)
			return
		case "setAutoSwap":
			newAmount, err := strconv.ParseUint(r.FormValue("thresholdAmount"), 10, 64)
			if err != nil {
				redirectWithError(w, r, "/liquid?", err)
				return
			}

			newPPM, err := strconv.ParseUint(r.FormValue("thresholdPPM"), 10, 64)
			if err != nil {
				redirectWithError(w, r, "/liquid?", err)
				return
			}

			newPct, err := strconv.ParseUint(r.FormValue("targetPct"), 10, 64)
			if err != nil {
				redirectWithError(w, r, "/liquid?", err)
				return
			}

			nowEnabled := r.FormValue("autoSwapEnabled") == "on"
			t := "Automatic swap-ins "
			msg := ""

			// Log only if something changed
			if nowEnabled && (!config.Config.AutoSwapEnabled ||
				config.Config.AutoSwapThresholdAmount != newAmount ||
				config.Config.AutoSwapThresholdPPM != newPPM ||
				config.Config.AutoSwapTargetPct != newPct) {
				t += "Enabled"
				msg = t
				log.Println(t)
			}

			if config.Config.AutoSwapEnabled && !nowEnabled {
				t += "Disabled"
				msg = t
				log.Println(t)
			}

			config.Config.AutoSwapThresholdPPM = newPPM
			config.Config.AutoSwapThresholdAmount = newAmount
			config.Config.AutoSwapTargetPct = newPct
			config.Config.AutoSwapEnabled = nowEnabled

			// Save config
			if err := config.Save(); err != nil {
				log.Println("Error saving config file:", err)
				redirectWithError(w, r, "/liquid?", err)
				return
			}

			if msg == "" {
				// Reload liquid page
				http.Redirect(w, r, "/liquid", http.StatusSeeOther)
			} else {
				// Go to home page with pop-up
				http.Redirect(w, r, "/?msg="+msg, http.StatusSeeOther)
			}
			return

		case "newAddress":
			res, err := ps.LiquidGetAddress(client)
			if err != nil {
				log.Printf("unable to connect to RPC server: %v", err)
				redirectWithError(w, r, "/liquid?", err)
				return
			}

			// Redirect to liquid page with new address
			http.Redirect(w, r, "/liquid?addr="+res.Address, http.StatusSeeOther)
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
			http.Redirect(w, r, "/liquid?txid="+txid, http.StatusSeeOther)
			return
		case "addPeer":
			nodeId := r.FormValue("nodeId")
			_, err := ps.AddPeer(client, nodeId)
			if err != nil {
				redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
				return
			}
			// Redirect to peer page
			http.Redirect(w, r, "/peer?id="+nodeId, http.StatusSeeOther)
			return

		case "removePeer":
			nodeId := r.FormValue("nodeId")
			_, err := ps.RemovePeer(client, nodeId)
			if err != nil {
				redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
				return
			}
			// Redirect to peer page
			http.Redirect(w, r, "/peer?id="+nodeId, http.StatusSeeOther)
			return

		case "suspectPeer":
			nodeId := r.FormValue("nodeId")
			_, err := ps.AddSusPeer(client, nodeId)
			if err != nil {
				redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
				return
			}
			// Redirect to peer page
			http.Redirect(w, r, "/peer?id="+nodeId, http.StatusSeeOther)
			return

		case "unsuspectPeer":
			nodeId := r.FormValue("nodeId")
			_, err := ps.RemoveSusPeer(client, nodeId)
			if err != nil {
				redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
				return
			}
			// Redirect to peer page
			http.Redirect(w, r, "/peer?id="+nodeId, http.StatusSeeOther)
			return

		case "doSwap":
			nodeId := r.FormValue("nodeId")
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

			var id string
			switch r.FormValue("direction") {
			case "swapIn":
				id, err = ps.SwapIn(client, swapAmount, channelId, r.FormValue("asset"), false)
			case "swapOut":
				id, err = ps.SwapOut(client, swapAmount, channelId, r.FormValue("asset"), false)
			}

			if err != nil {
				e := err.Error()
				if e == "Request timed out" || strings.HasPrefix(e, "rpc error: code = Unavailable desc = rpc timeout reached") {
					// sometimes the swap is pending anyway
					res, er := ps.ListActiveSwaps(client)
					if er != nil {
						log.Println("ListActiveSwaps:", er)
						redirectWithError(w, r, "/peer?id="+nodeId+"&", er)
						return
					}
					activeSwaps := res.GetSwaps()
					if len(activeSwaps) == 1 {
						// follow this id
						id = activeSwaps[0].Id
					} else {
						// display the original error
						log.Println("doSwap:", err)
						redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
						return
					}
				} else {
					log.Println("doSwap:", err)
					redirectWithError(w, r, "/peer?id="+nodeId+"&", err)
					return
				}
			}
			// Redirect to swap page to follow the swap
			http.Redirect(w, r, "/swap?id="+id, http.StatusSeeOther)

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

		secureConnection, err := strconv.ParseBool(r.FormValue("secureConnection"))
		if err != nil {
			redirectWithError(w, r, "/config?", err)
			return
		}

		// display CA certificate installation instructions
		if secureConnection && !config.Config.SecureConnection {
			config.Config.ServerIPs = r.FormValue("serverIPs")
			config.Save()
			http.Redirect(w, r, "/ca", http.StatusSeeOther)
			return
		}

		if r.FormValue("serverIPs") != config.Config.ServerIPs {
			config.Config.ServerIPs = r.FormValue("serverIPs")
			if secureConnection {
				if err := config.GenereateServerCertificate(); err == nil {
					config.Save()
					restart(w, r)
				} else {
					log.Println("GenereateServerCertificate:", err)
					redirectWithError(w, r, "/config?", err)
					return
				}
			}
		}

		// restart to listen on HTTP
		if !secureConnection && config.Config.SecureConnection {
			config.Config.SecureConnection = false
			config.Save()
			restart(w, r)
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

		rpcHost := r.FormValue("rpcHost")
		clientIsDown := false

		client, cleanup, err := ps.GetClient(rpcHost)
		if err != nil {
			clientIsDown = true
		} else {
			defer cleanup()
			_, err = ps.AllowSwapRequests(client, allowSwapRequests)
			if err != nil {
				// RPC Host entered is bad
				clientIsDown = true
			} else { // values are good, save them
				config.Config.RpcHost = rpcHost
				config.Config.AllowSwapRequests = allowSwapRequests
			}
		}

		if err := config.Save(); err != nil {
			log.Println("Error saving config file:", err)
			redirectWithError(w, r, "/config?", err)
			return
		}

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
		ErrorMessage   string
		PopUpMessage   string
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
		ErrorMessage:   "",
		PopUpMessage:   "",
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
		ErrorMessage   string
		PopUpMessage   string
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
		ErrorMessage:   "",
		PopUpMessage:   "",
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
	t := fmt.Sprint(err)
	// translate common errors into plain English
	switch {
	case strings.HasPrefix(t, "rpc error: code = Unavailable desc = connection error"):
		t = "Cannot connect to peerswapd. It either has not started listening yet or PeerSwap Host parameter is wrong. Check logs."
	case strings.HasPrefix(t, "Unable to dial socket"):
		t = "Cannot connect to lightningd. It either failed to start or has wrong configuration. Check logs."
	case strings.HasPrefix(t, "-32601:Unknown command 'peerswap-reloadpolicy'"):
		t = "Peerswap plugin is not installed or has wrong configuration. Check .lightning/config."
	case strings.HasPrefix(t, "rpc error: code = "):
		i := strings.Index(t, "desc =")
		if i > 0 {
			t = t[i+7:]
		}
	}
	// display the error to the web page header
	msg := url.QueryEscape(t)
	http.Redirect(w, r, redirectUrl+"err="+msg, http.StatusSeeOther)
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

	// execute Automatic Swap In
	if config.Config.AutoSwapEnabled {
		go executeAutoSwap()
	}

	// LND: download and subscribe to invoices, forwards and payments
	// CLN: fetch swap fees paid and received via LN
	go ln.SubscribeAll()
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

	// Save config
	if err := config.Save(); err != nil {
		log.Println("Error saving config file:", err)
		return
	}
}

func bitcoinHandler(w http.ResponseWriter, r *http.Request) {
	//check for error message to display
	errorMessage := ""
	keys, ok := r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		errorMessage = keys[0]
	}

	//check for pop-up message to display
	popupMessage := ""
	keys, ok = r.URL.Query()["msg"]
	if ok && len(keys[0]) > 0 {
		popupMessage = keys[0]
	}

	var utxos []ln.UTXO
	cl, clean, er := ln.GetClient()
	if er != nil {
		redirectWithError(w, r, "/config?", er)
		return
	}
	defer clean()

	type Page struct {
		ErrorMessage     string
		PopUpMessage     string
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
		LiquidFeeRate    float64
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
		ErrorMessage:     errorMessage,
		PopUpMessage:     popupMessage,
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
		LiquidFeeRate:    liquid.EstimateFee(),
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
				if err := config.Save(); err != nil {
					log.Println("Error saving config file:", err)
					redirectWithError(w, r, "/bitcoin?", err)
					return
				}
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

		config.Config.PeginClaimScript = addr.ClaimScript
		config.Config.PeginAddress = addr.MainChainAddress
		config.Config.PeginAmount = res.AmountSat
		config.Config.PeginTxId = res.TxId
		config.Config.PeginReplacedTxId = ""
		config.Config.PeginFeeRate = uint32(fee)

		if err := config.Save(); err != nil {
			log.Println("Error saving config file:", err)
			redirectWithError(w, r, "/bitcoin?", err)
			return
		}

		telegramSendMessage("⏰ Started peg-in " + formatWithThousandSeparators(uint64(res.AmountSat)) + " sats. Time left: " + formattedDuration)

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

		if ln.CanRBF() {
			// to speed things up, also broadcast it to mempool.space
			internet.SendRawTransaction(res.RawHex)

			log.Println("RBF TxId:", res.TxId)
			config.Config.PeginReplacedTxId = config.Config.PeginTxId
			config.Config.PeginAmount = res.AmountSat
			config.Config.PeginTxId = res.TxId
		} else {
			// txid not available, let's hope LND broadcasted it fine
			log.Println("CPFP initiated")
		}

		// save the new rate, so the next bump cannot be lower
		config.Config.PeginFeeRate = uint32(fee)

		if err := config.Save(); err != nil {
			log.Println("Error saving config file:", err)
			redirectWithError(w, r, "/bitcoin?", err)
			return
		}

		// Redirect to bitcoin page to follow the pegin progress
		http.Redirect(w, r, "/bitcoin?msg=New transaction broadcasted", http.StatusSeeOther)
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
func convertPeersToHTMLTable(
	peers []*peerswaprpc.PeerSwapPeer,
	allowlistedPeers []string,
	suspiciousPeers []string,
	swaps []*peerswaprpc.PrettyPrintSwap,
	outboundFeeRates map[uint64]int64,
	inboundFeeRates map[uint64]int64,
	showAll bool) string {

	type Table struct {
		AvgLocal int
		HtmlBlob string
	}

	// find last swap timestamps per channel
	swapTimestamps := make(map[uint64]int64)

	for _, swap := range swaps {
		if simplifySwapState(swap.State) == "success" && swapTimestamps[swap.LndChanId] < swap.CreatedAt {
			swapTimestamps[swap.LndChanId] = swap.CreatedAt
		}
	}

	var unsortedTable []Table

	for _, peer := range peers {
		var totalLocal float64
		var totalCapacity float64
		var totalForwardsOut uint64
		var totalForwardsIn uint64
		var totalPayments uint64
		var totalFees uint64
		var totalCost uint64

		channelsTable := "<table style=\"table-layout: fixed; width: 100%; margin-bottom: 0.5em;\">"

		// Construct channels data
		for _, channel := range peer.Channels {
			// red background for inactive channels
			bc := "#590202"
			fc := "grey"
			if config.Config.ColorScheme == "light" {
				bc = "#fcb6b6"
				fc = "grey"
			}

			if channel.Active {
				// green background for active channels
				bc = "#224725"
				fc = "white"
				if config.Config.ColorScheme == "light" {
					bc = "#e6ffe8"
					fc = "black"
				}
			}

			channelsTable += "<tr style=\"background-color: " + bc + "\"; >"
			channelsTable += feeInputField(peer.NodeId, channel.ChannelId, "outbound", outboundFeeRates[channel.ChannelId], bc, fc, showAll)
			channelsTable += "<td title=\"Local balance: " + formatWithThousandSeparators(channel.LocalBalance) + "\" id=\"scramble\" style=\"padding: 0px; width: 6ch; text-align: center\">"
			channelsTable += toMil(channel.LocalBalance)
			channelsTable += "</td><td style=\"padding: 0px; text-align: center; vertical-align: middle;\">"
			channelsTable += "<a href=\"/peer?id=" + peer.NodeId + "\">"

			local := float64(channel.LocalBalance)
			capacity := float64(channel.LocalBalance + channel.RemoteBalance)
			totalLocal += local
			totalCapacity += capacity
			tooltip := "In the last 6 months"

			// timestamp of the last swap or 6 months horizon
			lastSwapTimestamp := time.Now().AddDate(0, -6, 0).Unix()
			if swapTimestamps[channel.ChannelId] > lastSwapTimestamp {
				lastSwapTimestamp = swapTimestamps[channel.ChannelId]
				tooltip = "Since the last swap " + timePassedAgo(time.Unix(lastSwapTimestamp, 0).UTC())
			}

			stats := ln.GetChannelStats(channel.ChannelId, uint64(lastSwapTimestamp))
			totalFees += stats.FeeSat
			outflows := stats.RoutedOut + stats.PaidOut
			inflows := stats.RoutedIn + stats.InvoicedIn
			totalForwardsOut += stats.RoutedOut
			totalForwardsIn += stats.RoutedIn
			totalCost += stats.PaidCost
			totalPayments += stats.PaidOut

			netFlow := float64(int64(inflows) - int64(outflows))

			bluePct := int(local * 100 / capacity)
			greenPct := int(0)
			redPct := int(0)
			previousBlue := bluePct
			previousRed := redPct

			tooltip = fmt.Sprintf("%d", bluePct) + "% local balance\n" + tooltip + ":"
			flowText := ""
			if stats.RoutedIn > 0 {
				flowText += "\nRouted in: +" + formatWithThousandSeparators(stats.RoutedIn)
			}
			if stats.RoutedOut > 0 {
				flowText += "\nRouted out: -" + formatWithThousandSeparators(stats.RoutedOut)
			}
			if stats.InvoicedIn > 0 {
				flowText += "\nInvoiced in: +" + formatWithThousandSeparators(stats.InvoicedIn)
			}
			if stats.PaidOut > 0 {
				flowText += "\nPaid out: -" + formatWithThousandSeparators(stats.PaidOut)
			}

			if netFlow > 0 {
				greenPct = int(local * 100 / capacity)
				bluePct = int((local - netFlow) * 100 / capacity)
				previousBlue = greenPct
				flowText += "\nNet flow: +" + formatWithThousandSeparators(uint64(netFlow))
			}

			if netFlow < 0 {
				bluePct = int(local * 100 / capacity)
				redPct = int((local - netFlow) * 100 / capacity)
				previousRed = bluePct
				flowText += "\nNet flow: -" + formatWithThousandSeparators(uint64(-netFlow))
			}

			if flowText == "" {
				flowText = "\nNo flows"
			}

			if stats.FeeSat > 0 {
				flowText += "\nRevenue: +" + formatWithThousandSeparators(stats.FeeSat)
				if stats.RoutedOut > 0 {
					flowText += "\nRevenue PPM: " + formatWithThousandSeparators(stats.FeeSat*1_000_000/stats.RoutedOut)
				}
			}

			if stats.PaidCost > 0 {
				flowText += "\nCosts: -" + formatWithThousandSeparators(stats.PaidCost)
				if stats.PaidOut > 0 {
					flowText += "\nCosts PPM: " + formatWithThousandSeparators(stats.PaidCost*1_000_000/stats.PaidOut)
				}
			}

			tooltip += flowText

			currentProgress := fmt.Sprintf("%d%% 100%%, %d%% 100%%, %d%% 100%%, 100%% 100%%", bluePct, redPct, greenPct)
			previousProgress := fmt.Sprintf("%d%% 100%%, %d%% 100%%, %d%% 100%%, 100%% 100%%", previousBlue, previousRed, greenPct)

			channelsTable += "<div title=\"" + tooltip + "\" class=\"progress\" style=\"background-size: " + currentProgress + ";\" onmouseover=\"this.style.backgroundSize = '" + previousProgress + "';\" onmouseout=\"this.style.backgroundSize = '" + currentProgress + "';\"></div>"
			channelsTable += "</a></td>"
			channelsTable += "<td title=\"Remote balance: " + formatWithThousandSeparators(channel.RemoteBalance) + "\" id=\"scramble\" style=\"padding: 0px; width: 6ch; text-align: center\">"
			channelsTable += toMil(channel.RemoteBalance)
			channelsTable += "</td>"
			channelsTable += feeInputField(peer.NodeId, channel.ChannelId, "inbound", inboundFeeRates[channel.ChannelId], bc, fc, showAll)
			channelsTable += "</tr>"
		}
		channelsTable += "</table>"

		// count total outbound to sort peers later
		pct := int(1000000 * totalLocal / totalCapacity)

		peerTable := "<table style=\"table-layout:fixed; width: 100%\">"
		peerTable += "<tr style=\"border: 1px dotted;\">"
		peerTable += "<td class=\"truncate\" id=\"scramble\" style=\"padding: 0px; padding-left: 1px; float: left; text-align: left; width: 70%;\">"

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

		peerTable += "</td><td id=\"scramble\" style=\"padding: 0px; float: center; text-align: center; width:10ch;\">"

		ppmRevenue := uint64(0)
		ppmCost := uint64(0)
		if totalForwardsOut > 0 {
			ppmRevenue = totalFees * 1_000_000 / totalForwardsOut
		}
		if totalPayments > 0 {
			ppmCost = totalCost * 1_000_000 / totalPayments
		}

		peerTable += "<span title=\"Routing revenue since the last swap or for the previous 6 months. PPM: " + formatWithThousandSeparators(ppmRevenue) + "\">" + formatWithThousandSeparators(totalFees) + "</span>"
		if totalCost > 0 {
			peerTable += "<span title=\"Lighting costs since the last swap or in the last 6 months. PPM: " + formatWithThousandSeparators(ppmCost) + "\" style=\"color:red\"> -" + formatWithThousandSeparators(totalCost) + "</span>"
		}
		peerTable += "</td><td style=\"padding: 0px; padding-right: 1px; float: right; text-align: right; width:8ch;\">"

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

// converts a list of non-PS peers into an HTML table to display
func convertOtherPeersToHTMLTable(peers []*peerswaprpc.PeerSwapPeer,
	outboundFeeRates map[uint64]int64,
	inboundFeeRates map[uint64]int64,
	showAll bool) string {

	type Table struct {
		AvgLocal int
		HtmlBlob string
	}

	var unsortedTable []Table

	// timestamp of 6 months horizon
	lastSwapTimestamp := time.Now().AddDate(0, -6, 0).Unix()

	for _, peer := range peers {
		var totalLocal float64
		var totalCapacity float64
		var totalForwardsOut uint64
		var totalForwardsIn uint64
		var totalPayments uint64
		var totalFees uint64
		var totalCost uint64

		channelsTable := "<table style=\"table-layout: fixed; width: 100%; margin-bottom: 0.5em;\">"

		// Construct channels data
		for _, channel := range peer.Channels {
			// red background for inactive channels
			bc := "#590202"
			fc := "grey"
			if config.Config.ColorScheme == "light" {
				bc = "#fcb6b6"
				fc = "grey"
			}

			if channel.Active {
				// green background for active channels
				bc = "#224725"
				fc = "white"
				if config.Config.ColorScheme == "light" {
					bc = "#e6ffe8"
					fc = "black"
				}
			}

			channelsTable += "<tr style=\"background-color: " + bc + "\"; >"
			channelsTable += feeInputField(peer.NodeId, channel.ChannelId, "outbound", outboundFeeRates[channel.ChannelId], bc, fc, showAll)
			channelsTable += "<td title=\"Local balance: " + formatWithThousandSeparators(channel.LocalBalance) + "\" id=\"scramble\" style=\"padding: 0px; width: 6ch; text-align: center\">"
			channelsTable += toMil(channel.LocalBalance)
			channelsTable += "</td><td style=\"padding: 0px; text-align: center; vertical-align: middle;\">"
			channelsTable += "<a href=\"/peer?id=" + peer.NodeId + "\">"

			local := float64(channel.LocalBalance)
			capacity := float64(channel.LocalBalance + channel.RemoteBalance)
			totalLocal += local
			totalCapacity += capacity
			tooltip := "In the last 6 months"

			stats := ln.GetChannelStats(channel.ChannelId, uint64(lastSwapTimestamp))
			totalFees += stats.FeeSat
			outflows := stats.RoutedOut + stats.PaidOut
			inflows := stats.RoutedIn + stats.InvoicedIn
			totalForwardsOut += stats.RoutedOut
			totalForwardsIn += stats.RoutedIn
			totalPayments += stats.PaidOut
			totalCost += stats.PaidCost

			netFlow := float64(int64(inflows) - int64(outflows))

			bluePct := int(local * 100 / capacity)
			greenPct := int(0)
			redPct := int(0)
			previousBlue := bluePct
			previousRed := redPct

			tooltip = fmt.Sprintf("%d", bluePct) + "% local balance\n" + tooltip + ":"
			flowText := ""

			if stats.RoutedIn > 0 {
				flowText += "\nRouted in: +" + formatWithThousandSeparators(stats.RoutedIn)
			}
			if stats.RoutedOut > 0 {
				flowText += "\nRouted out: -" + formatWithThousandSeparators(stats.RoutedOut)
			}
			if stats.InvoicedIn > 0 {
				flowText += "\nInvoiced in: +" + formatWithThousandSeparators(stats.InvoicedIn)
			}
			if stats.PaidOut > 0 {
				flowText += "\nPaid out: -" + formatWithThousandSeparators(stats.PaidOut)
			}

			if netFlow > 0 {
				greenPct = int(local * 100 / capacity)
				bluePct = int((local - netFlow) * 100 / capacity)
				previousBlue = greenPct
				flowText += "\nNet flow: +" + formatWithThousandSeparators(uint64(netFlow))
			}

			if netFlow < 0 {
				bluePct = int(local * 100 / capacity)
				redPct = int((local - netFlow) * 100 / capacity)
				previousRed = bluePct
				flowText += "\nNet flow: -" + formatWithThousandSeparators(uint64(-netFlow))
			}

			if flowText == "" {
				flowText = "\nNo flows"
			}

			tooltip += flowText

			currentProgress := fmt.Sprintf("%d%% 100%%, %d%% 100%%, %d%% 100%%, 100%% 100%%", bluePct, redPct, greenPct)
			previousProgress := fmt.Sprintf("%d%% 100%%, %d%% 100%%, %d%% 100%%, 100%% 100%%", previousBlue, previousRed, greenPct)

			channelsTable += "<div title=\"" + tooltip + "\" class=\"progress\" style=\"background-size: " + currentProgress + ";\" onmouseover=\"this.style.backgroundSize = '" + previousProgress + "';\" onmouseout=\"this.style.backgroundSize = '" + currentProgress + "';\"></div>"
			channelsTable += "</a></td>"
			channelsTable += "<td title=\"Remote balance: " + formatWithThousandSeparators(channel.RemoteBalance) + "\" id=\"scramble\" style=\"padding: 0px; width: 6ch; text-align: center\">"
			channelsTable += toMil(channel.RemoteBalance)
			channelsTable += "</td>"
			channelsTable += feeInputField(peer.NodeId, channel.ChannelId, "inbound", inboundFeeRates[channel.ChannelId], bc, fc, showAll)
			channelsTable += "</tr>"
		}
		channelsTable += "</table>"

		// count total outbound to sort peers later
		pct := int(1000000 * totalLocal / totalCapacity)

		peerTable := "<table style=\"table-layout:fixed; width: 100%\">"
		peerTable += "<tr style=\"border: 1px dotted;\">"
		peerTable += "<td class=\"truncate\" id=\"scramble\" style=\"padding: 0px; padding-left: 1px; float: left; text-align: left; width: 70%;\">"

		// alias is a link to open peer details page
		peerTable += "<a href=\"/peer?id=" + peer.NodeId + "\">"

		peerTable += "<span title=\"Not using PeerSwap\">🙁&nbsp</span>"
		peerTable += "<span title=\"Invite peer to PeerSwap via a direct Keysend message\">" + getNodeAlias(peer.NodeId)
		peerTable += "</span></a>"

		peerTable += "</td><td id=\"scramble\" style=\"padding: 0px; float: center; text-align: center; width:10ch;\">"

		ppmRevenue := uint64(0)
		ppmCost := uint64(0)
		if totalForwardsOut > 0 {
			ppmRevenue = totalFees * 1_000_000 / totalForwardsOut
		}
		peerTable += "<span title=\"Routing revenue for the previous 6 months. PPM: " + formatWithThousandSeparators(ppmRevenue) + "\">" + formatWithThousandSeparators(totalFees) + "</span> "
		if totalPayments > 0 {
			ppmCost = totalCost * 1_000_000 / totalPayments
		}
		if totalCost > 0 {
			peerTable += "<span title=\"Lighting costs in the last 6 months. PPM: " + formatWithThousandSeparators(ppmCost) + "\" style=\"color:red\"> -" + formatWithThousandSeparators(totalCost) + "</span>"
		}

		peerTable += "</td><td style=\"padding: 0px; padding-right: 1px; float: right; text-align: right; width:10ch;\">"
		peerTable += "<a title=\"Invite peer to PeerSwap via a direct Keysend message\" href=\"/peer?id=" + peer.NodeId + "\">Invite&nbsp</a>"
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
// if nodeId, swapState, swapRole != "" then only show swaps for that filter
func convertSwapsToHTMLTable(swaps []*peerswaprpc.PrettyPrintSwap, nodeId string, swapState string, swapRole string) string {

	if len(swaps) == 0 {
		return ""
	}

	type Table struct {
		TimeStamp int64
		HtmlBlob  string
	}
	var (
		unsortedTable []Table
		totalAmount   uint64
		totalCost     int64
	)

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
		table += "<td style=\"width: 30%; text-align: left; padding-bottom: 0.5em;\">"

		tm := timePassedAgo(time.Unix(swap.CreatedAt, 0).UTC())

		// clicking on timestamp will open swap details page
		table += "<a title=\"Open swap details page\" href=\"/swap?id=" + swap.Id + "\">" + tm + "</a> "
		table += "</td><td style=\"text-align: left\">"

		// clicking on swap status will filter swaps with equal status
		state := simplifySwapState(swap.State)
		table += "<a title=\"Filter by state: " + state + "\" href=\"/?id=" + nodeId + "&state=" + simplifySwapState(swap.State) + "&role=" + swapRole + "\">"
		table += visualiseSwapState(swap.State, false) + "&nbsp</a>"
		table += " <span title=\"Swap amount, sats\">" + formatWithThousandSeparators(swap.Amount) + "</span>"

		if state == "success" {
			totalAmount += swap.Amount
		}

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

		cost := swapCost(swap)
		if cost != 0 {
			totalCost += cost
			ppm := cost * 1_000_000 / int64(swap.Amount)
			table += " <span title=\"Swap cost, sats. PPM: " + formatSigned(ppm) + "\">" + formatSigned(cost) + "</span>"
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

	// show total amount and cost
	ppm := int64(0)
	if totalAmount > 0 {
		ppm = totalCost * 1_000_000 / int64(totalAmount)
	}

	table += "<p style=\"text-align: center\">Total: " + toMil(totalAmount) + ", Cost: " + formatSigned(totalCost) + " sats, PPM: " + formatSigned(ppm) + "</p"

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

		if err := config.Save(); err != nil {
			log.Println("Error saving config file:", err)
		}
	}
}

func getNodeAlias(key string) string {
	// search in cache
	alias, exists := aliasCache[key]
	if exists {
		return alias
	}

	// try lightning
	alias = ln.GetAlias(key)

	if alias == "" {
		// try mempool
		alias = internet.GetNodeAlias(key)
	}

	if alias == "" {
		// return first 20 chars of key
		return key[:20]
	}

	// save to cache if alias was found
	aliasCache[key] = alias

	return alias
}

// preemptively load all Aliases in cache
func cacheAliases() {
	// Lightning RPC client
	cl, clean, er := ln.GetClient()
	if er != nil {
		return
	}
	defer clean()

	res, err := ln.ListPeers(cl, "", nil)
	if err != nil {
		return
	}

	peers := res.GetPeers()
	for _, peer := range peers {
		getNodeAlias(peer.NodeId)
	}
}

// Finds a candidate for an automatic swap-in
// The goal is to spend maximum available liquid
// To rebalance a channel with high enough historic fee PPM
func findSwapInCandidate(candidate *SwapParams) error {
	// extra 1000 to avoid no-change tx spending all on fees
	minAmount := config.Config.AutoSwapThresholdAmount - ln.SwapFeeReserveLBTC - 1000
	minPPM := config.Config.AutoSwapThresholdPPM

	client, cleanup, err := ps.GetClient(config.Config.RpcHost)
	if err != nil {
		return err
	}
	defer cleanup()

	res, err := ps.ListPeers(client)
	if err != nil {
		return err
	}
	peers := res.GetPeers()

	res2, err := ps.ListSwaps(client)
	if err != nil {
		return err
	}
	swaps := res2.GetSwaps()

	// find last swap timestamps per channel
	swapTimestamps := make(map[uint64]int64)

	for _, swap := range swaps {
		if simplifySwapState(swap.State) == "success" && swapTimestamps[swap.LndChanId] < swap.CreatedAt {
			swapTimestamps[swap.LndChanId] = swap.CreatedAt
		}
	}

	cl, clean, err := ln.GetClient()
	if err != nil {
		return err
	}
	defer clean()

	for _, peer := range peers {
		// ignore peer with Liquid swaps disabled
		if !peer.SwapsAllowed || !stringIsInSlice("lbtc", peer.SupportedAssets) {
			continue
		}
		for _, channel := range peer.Channels {
			chanInfo := ln.GetChannelInfo(cl, channel.ChannelId, peer.NodeId)
			// find the potential swap amount to bring balance to target
			targetBalance := chanInfo.Capacity * config.Config.AutoSwapTargetPct / 100

			// limit target to Remote - reserve
			reserve := chanInfo.Capacity / 100
			targetBalance = min(targetBalance, chanInfo.Capacity-reserve)

			if targetBalance < channel.LocalBalance {
				continue
			}

			swapAmount := targetBalance - channel.LocalBalance

			// limit to max HTLC setting
			swapAmount = min(swapAmount, chanInfo.PeerMaxHtlcMsat/1000)
			swapAmount = min(swapAmount, chanInfo.OurMaxHtlcMsat/1000)

			// only consider active channels with enough remote balance
			if channel.Active && swapAmount >= minAmount {
				// use timestamp of the last swap or 6 months horizon
				lastSwapTimestamp := time.Now().AddDate(0, -6, 0).Unix()
				if swapTimestamps[channel.ChannelId] > lastSwapTimestamp {
					lastSwapTimestamp = swapTimestamps[channel.ChannelId]
				}

				stats := ln.GetChannelStats(channel.ChannelId, uint64(lastSwapTimestamp))

				ppm := uint64(0)
				if stats.RoutedOut > 0 {
					ppm = stats.FeeSat * 1_000_000 / stats.RoutedOut
				}

				// aim to maximize accumulated PPM
				// if ppm ties, choose the candidate with larger potential swap amount
				if ppm > minPPM || ppm == minPPM && swapAmount > candidate.Amount {
					// set the candidate's PPM as the new target to beat
					minPPM = ppm
					// save the candidate
					candidate.ChannelId = channel.ChannelId
					candidate.PeerAlias = getNodeAlias(peer.NodeId)
					// set maximum possible amount
					candidate.Amount = swapAmount
					candidate.PPM = ppm
				}
			}
		}
	}
	return nil
}

func executeAutoSwap() {
	client, cleanup, err := ps.GetClient(config.Config.RpcHost)
	if err != nil {
		return
	}
	defer cleanup()

	res, err := ps.LiquidGetBalance(client)
	if err != nil {
		return
	}

	satAmount := res.GetSatAmount()

	if satAmount < config.Config.AutoSwapThresholdAmount {
		return
	}

	res2, err := ps.ListActiveSwaps(client)
	if err != nil {
		return
	}

	activeSwaps := res2.GetSwaps()

	// cannot have active swaps pending
	if len(activeSwaps) > 0 {
		return
	}

	var candidate SwapParams

	if err := findSwapInCandidate(&candidate); err != nil {
		// some error prevented candidate finding
		return
	}

	amount := candidate.Amount

	// no suitable candidates were found
	if amount == 0 {
		return
	}

	// extra 1000 reserve to avoid no-change tx spending all on fees
	amount = min(amount, satAmount-ln.SwapFeeReserveLBTC-1000)

	// execute swap
	id, err := ps.SwapIn(client, amount, candidate.ChannelId, "lbtc", false)
	if err != nil {
		log.Println("AutoSwap error:", err)
		return
	}

	// Log swap id
	log.Println("Initiated Auto Swap-In, id: "+id+", Peer: "+candidate.PeerAlias+", L-BTC Amount: "+formatWithThousandSeparators(amount)+", Channel's PPM: ", formatWithThousandSeparators(candidate.PPM))

	// Send telegram
	telegramSendMessage("🤖 Initiated Auto Swap-In with " + candidate.PeerAlias + " for " + formatWithThousandSeparators(amount) + " Liquid sats. Channel's PPM: " + formatWithThousandSeparators(candidate.PPM))
}

func swapCost(swap *peerswaprpc.PrettyPrintSwap) int64 {
	if swap == nil {
		return 0
	}

	if simplifySwapState(swap.State) != "success" {
		return 0
	}

	fee := int64(0)
	switch swap.Type + swap.Role {
	case "swap-outsender":
		fee = ln.SwapRebates[swap.Id]
	case "swap-insender":
		fee = onchainTxFee(swap.Asset, swap.OpeningTxId)
	case "swap-outreceiver":
		fee = onchainTxFee(swap.Asset, swap.OpeningTxId) - ln.SwapRebates[swap.Id]
	case "swap-inreceiver":
		fee = onchainTxFee(swap.Asset, swap.ClaimTxId)
	}

	return fee
}

// get tx fee from cache or online
func onchainTxFee(asset, txId string) int64 {
	// try cache
	fee, exists := txFee[txId]
	if exists {
		return fee
	}
	switch asset {
	case "lbtc":

		fee = internet.GetLiquidTxFee(txId)
	case "btc":
		fee = internet.GetBitcoinTxFee(txId)

	}
	// save to cache
	if fee > 0 {
		txFee[txId] = fee
	}
	return fee
}

func cacheSwapCosts() {
	client, cleanup, err := ps.GetClient(config.Config.RpcHost)
	if err != nil {
		return
	}
	defer cleanup()

	res, err := ps.ListSwaps(client)
	if err != nil {
		return
	}

	swaps := res.GetSwaps()

	for _, swap := range swaps {
		swapCost(swap)
	}
}
