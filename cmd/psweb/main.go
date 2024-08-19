package main

import (
	"crypto/tls"
	"crypto/x509"
	"embed"
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
	"peerswap-web/cmd/psweb/db"
	"peerswap-web/cmd/psweb/internet"
	"peerswap-web/cmd/psweb/liquid"
	"peerswap-web/cmd/psweb/ln"
	"peerswap-web/cmd/psweb/ps"

	"github.com/elementsproject/peerswap/peerswaprpc"
	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
)

const (
	// App version tag
	version = "v1.7.0"

	// Swap Out reserves are hardcoded here:
	// https://github.com/ElementsProject/peerswap/blob/c77a82913d7898d0d3b7c83e4a990abf54bd97e5/peerswaprpc/server.go#L105
	swapOutChannelReserve = 5000
	// https://github.com/ElementsProject/peerswap/blob/c77a82913d7898d0d3b7c83e4a990abf54bd97e5/swap/actions.go#L388
	swapOutChainReserve = 20300
	// Swap In reserves
	swapFeeReserveLBTC = uint64(300)
)

type SwapParams struct {
	PeerAlias string
	PeerId    string
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
	// Key used for cookie encryption
	store *sessions.CookieStore
	// catch a pending Auto Swap Id to check the state later
	autoSwapPending bool
	autoSwapId      string
	// store peer pub mapped to channel Id
	peerNodeId = make(map[uint64]string)
	// only poll all peers once after peerswap initializes
	initalPollComplete = false
	// identifies if this version of Elements Core supports discounted vSize
	hasDiscountedvSize = false
)

func main() {

	var (
		dataDir     = flag.String("datadir", "", "Path to config folder (default: ~/.peerswap)")
		password    = flag.String("password", "", "Enable HTTPS with password authentication (default: per pswebconfig.json)")
		showHelp    = flag.Bool("help", false, "Show help")
		showVersion = flag.Bool("version", false, "Show version")
	)

	flag.Parse()

	if *showHelp {
		fmt.Println("A lightweight server-side rendered Web UI for PeerSwap, which allows trustless p2p submarine swaps Lightning<->BTC and Lightning<->Liquid. Also facilitates BTC->Liquid pegins. PeerSwap with Liquid is a great cost efficient way to rebalance lightning channels.")
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

	if *password != "" {
		// enable HTTPS
		config.Config.SecureConnection = true
		config.Config.Password = *password
		config.Save()
	}

	if config.Config.SecureConnection && config.Config.Password != "" {
		// generate unique cookie
		cookie, err := config.GeneratePassword(10)
		if err != nil {
			log.Panicln("Cannot generate cookie store")
		}
		store = sessions.NewCookieStore([]byte(cookie))
	}

	// set logging params
	cleanup, err := setLogging()
	if err != nil {
		log.Fatal(err)
	}
	defer cleanup()

	// identify if Elements Core supports CT discounts
	hasDiscountedvSize = liquid.GetVersion() >= 230202
	// identify if liquid blockchain supports CT discounts
	liquidGenesisHash, err := liquid.GetBlockHash(0)
	if err == nil {
		hasDiscountedvSize = stringIsInSlice(liquidGenesisHash, []string{"6d955c95af04f1d14f5a6e6bd501508b37bddb6202aac6d99d0522a8cb7deef5"})
	}

	// Load persisted data from database
	ln.LoadDB()
	db.Load("Peers", "NodeId", &peerNodeId)
	db.Load("Swaps", "txFee", &txFee)

	// fetch all chain costs
	cacheSwapCosts()

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
			"ff":   formatFloat,
			"m":    toMil,
			"last": last,
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
	r.HandleFunc("/login", loginHandler)
	r.HandleFunc("/logout", logoutHandler)
	r.HandleFunc("/downloadca", downloadCaHandler)
	r.HandleFunc("/af", afHandler)

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

		go serveHTTPS(authMiddleware(r))
		log.Println("Listening HTTPS on port " + config.Config.SecurePort)
	} else {
		// Start HTTP server
		http.Handle("/", authMiddleware(r))
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

	// CLN: refresh forwarding stats
	go ln.CacheForwards()

	// Handle termination signals
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for termination signal
	sig := <-signalChan
	log.Printf("Received termination signal: %s\n", sig)

	// persist to db
	if db.Save("Swaps", "SwapRebates", ln.SwapRebates) != nil {
		log.Printf("Failed to persist SwapRebates to db")
	}

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

	if !fileExists(certFile) {
		// generate CA if does not exist
		if !fileExists(filepath.Join(config.Config.DataDir, "CA.crt")) {
			config.GenerateCA()
		}
		// generate from CA if does not exist
		config.GenerateServerCertificate()
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

	// Do not require client certificate if Password auth enabled
	if config.Config.Password != "" {
		tlsConfig.ClientAuth = tls.NoClientCert
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

func redirectWithError(w http.ResponseWriter, r *http.Request, redirectUrl string, err error) {
	t := fmt.Sprint(err)
	// translate common errors into plain English
	switch {
	case strings.HasPrefix(t, "rpc error: code = Unavailable desc = connection error"):
		t = "Peerswapd has not started listening yet or PeerSwap Host parameter is wrong. <a href='/log'>Check log</a>."
	case strings.HasPrefix(t, "Unable to dial socket"):
		t = "Lightningd failed to start or has wrong configuration. <a href='/log?log=cln.log'>Check log</a>."
	case strings.HasPrefix(t, "-32601:Unknown command 'peerswap-reloadpolicy'"):
		t = "Peerswap plugin is not installed or has wrong configuration. Check .lightning/config."
	case strings.HasPrefix(t, "rpc error: code = "):
		i := strings.Index(t, "desc =")
		if i > 0 {
			t = t[i+7:]
		}
	}
	// display the error to the web page header
	msg := url.QueryEscape(strings.ReplaceAll(t, `"`, `'`))
	http.Redirect(w, r, redirectUrl+"err="+msg, http.StatusSeeOther)
}

func startTimer() {
	// first run immediately
	onTimer(true)

	// then every minute
	for range time.Tick(60 * time.Second) {
		onTimer(false)
	}
}

// tasks that run every minute
func onTimer(firstRun bool) {
	// Start Telegram bot if not already running
	go telegramStart()

	// Back up to Telegram if Liquid balance changed
	liquidBackup(false)

	// check for updates
	go func() {
		t := internet.GetLatestTag()
		if t != "" {
			latestVersion = t
		}
	}()

	// refresh fee rate
	go func() {
		r := internet.GetFeeRate()
		if r > 0 {
			mempoolFeeRate = r
		}
	}()

	// execute Automatic Swap In
	if config.Config.AutoSwapEnabled {
		executeAutoSwap()
	}

	// LND: download and subscribe to invoices, forwards and payments
	// CLN: fetch swap fees paid and received via LN
	go ln.SubscribeAll()

	// execute auto fee
	if !firstRun {
		// skip first run so that forwards have time to download
		go ln.ApplyAutoFees()
		// Check if pegin can be claimed, initiated or joined
		checkPegin()
	}

	// advertise Liquid balance
	go advertiseBalances()

	// try every minute after startup until completed
	go pollBalances()
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

	// save the last wallet amount
	config.Config.ElementsBackupAmount = satAmount
	config.Save()
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

	// add new line after start up
	log.Println("")

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

			if stats.AssistedFeeSat > 0 {
				flowText += "\nAssisted Revenue: +" + formatWithThousandSeparators(stats.AssistedFeeSat)
				if stats.RoutedIn > 0 {
					flowText += "\nAssisted PPM: " + formatWithThousandSeparators(stats.AssistedFeeSat*1_000_000/stats.RoutedIn)
				}
			}

			if stats.PaidCost > 0 {
				flowText += "\nLightning Costs: -" + formatWithThousandSeparators(stats.PaidCost)
				if stats.PaidOut > 0 {
					flowText += "\nLightning Costs PPM: " + formatWithThousandSeparators(stats.PaidCost*1_000_000/stats.PaidOut)
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
		if len(peer.Channels) > 0 {
			peerTable += "<a href=\"/peer?id=" + peer.NodeId + "\">"
		} else {
			peerTable += "<a href=\"" + config.Config.NodeApi + "/" + peer.NodeId + "\" target=\"_blank\">"
		}

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
			color := "red"
			if config.Config.ColorScheme == "dark" {
				color = "pink"
			}
			peerTable += "<span title=\"Lightning costs since the last swap or in the last 6 months. PPM: " + formatWithThousandSeparators(ppmCost) + "\" style=\"color:" + color + "\"> -" + formatWithThousandSeparators(totalCost) + "</span>"
		}
		peerTable += "</td><td style=\"padding: 0px; padding-right: 1px; float: right; text-align: right; \">"

		if stringIsInSlice("btc", peer.SupportedAssets) {
			peerTable += "<span title=\"BTC swaps enabled\" style=\"color: #FF9900; font-weight: bold;\">₿</span>&nbsp"

			if ptr := ln.BitcoinBalances[peer.NodeId]; ptr != nil {
				btcBalance := ptr.Amount
				tm := timePassedAgo(time.Unix(ptr.TimeStamp, 0).UTC())
				flooredBalance := "<span style=\"color:grey\">0m</span>"
				if btcBalance > 100_000 {
					flooredBalance = toMil(btcBalance)
				}
				peerTable += "<span title=\"Peer's BTC balance: " + formatWithThousandSeparators(btcBalance) + " sats\nLast update: " + tm + "\">" + flooredBalance + "</span>"
			}
		}

		if stringIsInSlice("lbtc", peer.SupportedAssets) {
			peerTable += "<span title=\"L-BTC swaps enabled\">🌊</span>"

			if ptr := ln.LiquidBalances[peer.NodeId]; ptr != nil {
				lbtcBalance := ptr.Amount
				tm := timePassedAgo(time.Unix(ptr.TimeStamp, 0).UTC())
				flooredBalance := "<span style=\"color:grey\">0m</span>"
				if lbtcBalance > 100_000 {
					flooredBalance = toMil(lbtcBalance)
				}
				peerTable += "<span title=\"Peer's L-BTC balance: " + formatWithThousandSeparators(lbtcBalance) + " sats\nLast update: " + tm + "\">" + flooredBalance + "</span>"
			}
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

			if stats.FeeSat > 0 {
				flowText += "\nRevenue: +" + formatWithThousandSeparators(stats.FeeSat)
				if stats.RoutedOut > 0 {
					flowText += "\nRevenue PPM: " + formatWithThousandSeparators(stats.FeeSat*1_000_000/stats.RoutedOut)
				}
			}

			if stats.AssistedFeeSat > 0 {
				flowText += "\nAssisted Revenue: +" + formatWithThousandSeparators(stats.AssistedFeeSat)
				if stats.RoutedIn > 0 {
					flowText += "\nAssisted PPM: " + formatWithThousandSeparators(stats.AssistedFeeSat*1_000_000/stats.RoutedIn)
				}
			}

			if stats.PaidCost > 0 {
				flowText += "\nLightning Costs: -" + formatWithThousandSeparators(stats.PaidCost)
				if stats.PaidOut > 0 {
					flowText += "\nLightning Costs PPM: " + formatWithThousandSeparators(stats.PaidCost*1_000_000/stats.PaidOut)
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

		// peerTable += "<span title=\"Not using PeerSwap\">🙁&nbsp</span>"
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
			color := "red"
			if config.Config.ColorScheme == "dark" {
				color = "pink"
			}
			peerTable += "<span title=\"Lightning costs in the last 6 months. PPM: " + formatWithThousandSeparators(ppmCost) + "\" style=\"color:" + color + "\"> -" + formatWithThousandSeparators(totalCost) + "</span>"
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

	table += "<p style=\"text-align: center; white-space: nowrap\">Total: " + toMil(totalAmount) + ", Cost: " + formatSigned(totalCost) + " sats, PPM: " + formatSigned(ppm) + "</p>"

	return table
}

// Check Pegin status
func checkPegin() {
	cl, clean, er := ln.GetClient()
	if er != nil {
		return
	}
	defer clean()

	currentBlockHeight := ln.GetBlockHeight(cl)

	if currentBlockHeight > ln.JoinBlockHeight && ln.MyRole == "none" && ln.ClaimJoinHandler != "" {
		// invitation expired
		ln.ClaimStatus = "No ClaimJoin pegin is pending"
		log.Println("Invitation expired from", ln.ClaimJoinHandler)
		telegramSendMessage("🧬 ClaimJoin Invitation expired")

		ln.ClaimJoinHandler = ""
		db.Save("ClaimJoin", "ClaimStatus", ln.ClaimStatus)
		db.Save("ClaimJoin", "ClaimJoinHandler", ln.ClaimJoinHandler)
	}

	if config.Config.PeginTxId == "" {
		// send telegram if received new ClaimJoin invitation
		if peginInvite != ln.ClaimJoinHandler {
			t := "🧬 There is a ClaimJoin pegin pending"
			if ln.ClaimJoinHandler == "" {
				t = "🧬 ClaimJoin pegin has ended"
			} else {
				duration := time.Duration(10*(ln.JoinBlockHeight-currentBlockHeight)) * time.Minute
				formattedDuration := time.Time{}.Add(duration).Format("15h 04m")
				t += ", time limit to join: " + formattedDuration
			}
			if telegramSendMessage(t) {
				peginInvite = ln.ClaimJoinHandler
			}
		}
		return
	}

	if config.Config.PeginClaimJoin {
		if config.Config.PeginClaimScript == "done" {
			// finish by sending telegram message
			telegramSendMessage("🧬 ClaimJoin pegin successfull! Liquid TxId: `" + config.Config.PeginTxId + "`")
			config.Config.PeginClaimScript = ""
			config.Config.PeginTxId = ""
			config.Config.PeginClaimJoin = false
			config.Save()
			return
		}

		if ln.MyRole != "none" {
			// 10 blocks to wait before switching back to individual claim
			margin := uint32(10)
			if ln.MyRole == "initiator" && len(ln.ClaimParties) < 2 {
				// if no one has joined, switch on mturity
				margin = 0
			}
			if currentBlockHeight >= ln.ClaimBlockHeight+margin {
				// claim pegin individually
				t := "ClaimJoin expired, falling back to the individual claim"
				log.Println(t)
				telegramSendMessage("🧬 " + t)
				ln.MyRole = "none"
				config.Config.PeginClaimJoin = false
				config.Save()
				ln.EndClaimJoin("", "Reached Claim Block Height")
			} else if currentBlockHeight >= ln.ClaimBlockHeight && ln.MyRole == "initiator" {
				// proceed with
				ln.OnBlock(currentBlockHeight)
			}
			return
		}
	}

	confs, _ := ln.GetTxConfirmations(cl, config.Config.PeginTxId)
	if confs < 0 && config.Config.PeginReplacedTxId != "" {
		confs, _ = ln.GetTxConfirmations(cl, config.Config.PeginReplacedTxId)
		if confs > 0 {
			// RBF replacement conflict: the old transaction mined before the new one
			config.Config.PeginTxId = config.Config.PeginReplacedTxId
			config.Config.PeginReplacedTxId = ""
			log.Println("The last RBF failed as previous tx mined earlier, switching to prior txid:", config.Config.PeginTxId)
		}
	}

	if confs > 0 {
		if config.Config.PeginClaimScript == "" {
			log.Println("BTC withdrawal complete, txId: " + config.Config.PeginTxId)
			telegramSendMessage("💸 BTC withdrawal complete. TxId: `" + config.Config.PeginTxId + "`")
		} else if confs >= ln.PeginBlocks && ln.MyRole == "none" {
			// claim individual pegin
			failed := false
			proof := ""
			txid := ""
			rawTx, err := ln.GetRawTransaction(cl, config.Config.PeginTxId)
			if err == nil {
				proof, err = bitcoin.GetTxOutProof(config.Config.PeginTxId)
				if err == nil {
					txid, err = liquid.ClaimPegin(rawTx, proof, config.Config.PeginClaimScript)
					// claimpegin takes long time, allow it to timeout
					if err != nil && err.Error() != "timeout reading data from server" && err.Error() != "-4: Error: The transaction was rejected! Reason given: pegin-already-claimed" {
						failed = true
					}
				} else {
					failed = true
				}
			} else {
				failed = true
			}

			if failed {
				log.Println("Pegin claim FAILED!")
				log.Println("Mainchain TxId:", config.Config.PeginTxId)
				log.Println("Raw tx:", rawTx)
				log.Println("Proof:", proof)
				log.Println("Claim Script:", config.Config.PeginClaimScript)
				telegramSendMessage("❗ Pegin claim FAILED! See log for details.")
			} else {
				log.Println("Pegin successful! Liquid TxId:", txid)
				telegramSendMessage("💸 Pegin successfull! Liquid TxId: `" + txid + "`")
			}
		} else {
			if config.Config.PeginClaimJoin {
				if ln.MyRole == "none" {
					claimHeight := currentBlockHeight + ln.PeginBlocks - uint32(confs)
					if ln.ClaimJoinHandler == "" {
						// I will coordinate this join
						if ln.InitiateClaimJoin(claimHeight) {
							t := "Sent ClaimJoin invitations"
							log.Println(t + " as " + ln.MyPublicKey())
							telegramSendMessage("🧬 " + t)
							ln.MyRole = "initiator"
							db.Save("ClaimJoin", "MyRole", ln.MyRole)
						} else {
							log.Println("Failed to initiate ClaimJoin, continuing as a single pegin")
							config.Config.PeginClaimJoin = false
							config.Save()
						}
					} else if currentBlockHeight <= ln.JoinBlockHeight {
						// join by replying to initiator
						if ln.JoinClaimJoin(claimHeight) {
							t := "Applied to ClaimJoin group"
							log.Println(t + " " + ln.ClaimJoinHandler + " as " + ln.MyPublicKey())
							telegramSendMessage("🧬 " + t)
						} else {
							log.Println("Failed to apply to ClaimJoin group", ln.ClaimJoinHandler)
						}
					}
				}
			}
			return
		}

		// stop trying after one attempt
		config.Config.PeginTxId = ""
		config.Save()
	}
}

func getNodeAlias(key string) string {
	// search in cache
	alias, exists := aliasCache[key]
	if exists {
		return alias
	}

	if key == "" {
		return "* closed channel *"
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
	minAmount := config.Config.AutoSwapThresholdAmount - swapFeeReserveLBTC
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
	// true if initiated swap out or received swap in
	lastWasSwapOut := make(map[uint64]bool)

	for _, swap := range swaps {
		if simplifySwapState(swap.State) == "success" && swapTimestamps[swap.LndChanId] < swap.CreatedAt {
			swapTimestamps[swap.LndChanId] = swap.CreatedAt
			lastWasSwapOut[swap.LndChanId] = false
			if swap.Asset == "lbtc" && (swap.Type+swap.Role == "swap-outsender" || swap.Type+swap.Role == "swap-inreceiver") {
				lastWasSwapOut[swap.LndChanId] = true
			}
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
			// ignore if there was an opposite peerswap
			if lastWasSwapOut[channel.ChannelId] {
				continue
			}

			chanInfo := ln.GetChannelInfo(cl, channel.ChannelId, peer.NodeId)
			// find the potential swap amount to bring balance to target
			targetBalance := chanInfo.Capacity * config.Config.AutoSwapTargetPct / 100

			// limit target to Remote - reserve
			reserve := chanInfo.Capacity / 100
			targetBalance = min(targetBalance, chanInfo.Capacity-reserve)

			if targetBalance < channel.LocalBalance {
				continue
			}

			// only consider sink channels (net routing > 1k)
			lastSwapTimestamp := time.Now().AddDate(0, -6, 0).Unix()
			if swapTimestamps[channel.ChannelId] > lastSwapTimestamp {
				lastSwapTimestamp = swapTimestamps[channel.ChannelId]
			}

			stats := ln.GetChannelStats(channel.ChannelId, uint64(lastSwapTimestamp))
			if stats.RoutedOut-stats.RoutedIn <= 1000 {
				continue
			}

			swapAmount := targetBalance - channel.LocalBalance

			// limit to own and peer's max HTLC setting and remote balance less reserve for LN fee
			swapAmount = min(swapAmount, chanInfo.OurMaxHtlc, chanInfo.PeerMaxHtlc, channel.RemoteBalance-1000, config.Config.AutoSwapMaxAmount)

			// only consider active channels with enough remote balance
			if channel.Active && swapAmount >= minAmount {
				// use timestamp of the last swap or 6 months horizon
				lastTimestamp := time.Now().AddDate(0, -6, 0).Unix()
				if swapTimestamps[channel.ChannelId] > lastTimestamp {
					lastTimestamp = swapTimestamps[channel.ChannelId]
				}

				stats := ln.GetChannelStats(channel.ChannelId, uint64(lastTimestamp))

				ppm := uint64(0)
				if stats.RoutedOut > 1_000 { // ignore small results
					ppm = stats.FeeSat * 1_000_000 / stats.RoutedOut
				}

				// aim to maximize accumulated PPM
				// if ppm ties, choose the candidate with larger potential swap amount
				if ppm > minPPM || ppm == minPPM && swapAmount > candidate.Amount {
					// set the candidate's PPM as the new target to beat
					minPPM = ppm
					// save the candidate
					candidate.ChannelId = channel.ChannelId
					candidate.PeerId = peer.NodeId
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

	res2, err := ps.ListActiveSwaps(client)
	if err != nil {
		return
	}

	activeSwaps := res2.GetSwaps()

	if len(activeSwaps) > 0 {
		if autoSwapPending {
			// save the Id
			autoSwapId = activeSwaps[0].Id
		}
		// cannot have active swaps pending to initiate auto swap
		return
	}

	if autoSwapPending {
		disable := false
		// no active swaps means completed
		if autoSwapId != "" {
			// check the state
			res, err := ps.GetSwap(client, autoSwapId)
			if err != nil {
				log.Println("GetSwap:", err)
				disable = true
			}
			if res.GetSwap().State != "State_ClaimedPreimage" {
				log.Println("The last auto swap failed")
				disable = true
			}
		} else {
			// did not catch an Id is an exception
			disable = true
			log.Println("Unable to check the status of the last auto swap")
		}

		if disable {
			// disable auto swap
			config.Config.AutoSwapEnabled = false
			config.Save()
			log.Println("Automatic swap-ins Disabled")
		}

		// stop following
		autoSwapPending = false
		autoSwapId = ""
		return
	}

	res, err := ps.LiquidGetBalance(client)
	if err != nil {
		return
	}

	satAmount := res.GetSatAmount()

	if satAmount < config.Config.AutoSwapThresholdAmount {
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

	amount = min(amount, satAmount-swapFeeReserveLBTC)

	// execute swap
	id, err := ps.SwapIn(client, amount, candidate.ChannelId, "lbtc", false)
	if err != nil {
		log.Println("AutoSwap error:", err)
		return
	}

	// ready to catch Id and get status
	autoSwapPending = true
	autoSwapId = ""

	// Log swap id
	log.Println("Initiated Auto Swap-In, id: "+id+", Peer: "+candidate.PeerAlias+", L-BTC Amount: "+formatWithThousandSeparators(amount)+", Channel's PPM: ", formatWithThousandSeparators(candidate.PPM))

	// Send telegram
	telegramSendMessage("🤖 Initiated Auto Swap-In with " + candidate.PeerAlias + " for " + formatWithThousandSeparators(amount) + " Liquid sats. Channel's PPM: " + formatWithThousandSeparators(candidate.PPM))
}

func swapCost(swap *peerswaprpc.PrettyPrintSwap) int64 {
	if swap == nil {
		return 0
	}

	if !stringIsInSlice(swap.State, []string{"State_ClaimedPreimage", "State_ClaimedCoop", "State_ClaimedCsv"}) {
		return 0
	}

	fee := int64(0)
	switch swap.Type + swap.Role {
	case "swap-outsender":
		rebate, exists := ln.SwapRebates[swap.Id]
		if exists {
			fee = rebate
		}
	case "swap-insender":
		fee = onchainTxFee(swap.Asset, swap.OpeningTxId)
		if stringIsInSlice(swap.State, []string{"State_ClaimedCoop", "State_ClaimedCsv"}) {
			// swap failed but we bear the claim cost
			fee += onchainTxFee(swap.Asset, swap.ClaimTxId)
		}
	case "swap-outreceiver":
		fee = onchainTxFee(swap.Asset, swap.OpeningTxId)
		rebate, exists := ln.SwapRebates[swap.Id]
		if exists {
			fee -= rebate
		}
		if stringIsInSlice(swap.State, []string{"State_ClaimedCoop", "State_ClaimedCsv"}) {
			// swap failed but we bear the claim cost
			fee += onchainTxFee(swap.Asset, swap.ClaimTxId)
		}
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

	// save to db
	if db.Save("Swaps", "txFee", txFee) != nil {
		log.Printf("Failed to persist txFee to db")
	}
}

func restart(w http.ResponseWriter, r *http.Request, enableHTTPS bool, password string) {
	w.Header().Set("Content-Type", "text/html")
	host := strings.Split(r.Host, ":")[0]
	url := fmt.Sprintf("http://%s:%s", host, config.Config.ListenPort)

	html := fmt.Sprintf(`
		<!DOCTYPE html>
		<html lang="en">
		<head>
			<meta charset="UTF-8">
			<meta name="viewport" content="width=device-width, initial-scale=1.0">
			<title>Restarting...</title>
			<link rel="stylesheet" href="/static/bulma-%s.min.css">
			<link rel="stylesheet" href="/static/styles.css">
		</head>
		<body>
			<section class="section">
				<div class="container">
					<div class="columns is-centered">
						<div class="column is-4-desktop is-6-tablet is-12-mobile">
							<div class="box has-text-left">
								<p>PeerSwap Web UI is restarting...</p>
								<br>
								<p>Please navigate to <a href="%s">%s</a> to continue.</p>
							</div>
						</div>
					</div>
				</div>
			</section>
		</body>
		</html>`, config.Config.ColorScheme, url, url)

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, html)

	if !enableHTTPS && config.Config.Password != "" {
		// delete cookie
		session, _ := store.Get(r, "session")
		session.Values["authenticated"] = false
		session.Options.MaxAge = -1 // MaxAge < 0 means delete the cookie immediately.
		session.Save(r, w)
	}

	// Flush the response writer to ensure the message is sent before shutdown
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// Delay to ensure the message is displayed
	go func() {
		time.Sleep(1 * time.Second)

		config.Config.Password = password
		config.Config.SecureConnection = enableHTTPS
		config.Save()

		log.Println("Restart requested, stopping PSWeb.")
		// assume systemd will restart it
		os.Exit(0)
	}()
}

// NoOpWriter is an io.Writer that does nothing.
type NoOpWriter struct{}

// Write discards the data and returns success.
func (NoOpWriter) Write(p []byte) (n int, err error) {
	return len(p), nil
}

// NewMuteLogger creates a logger that discards all log output.
func NewMuteLogger() *log.Logger {
	return log.New(NoOpWriter{}, "", 0)
}

// html snippet to display and update fee PPM
func feeInputField(peerNodeId string, channelId uint64, direction string, feePerMil int64, backgroundColor string, fontColor string, showAll bool) string {

	channelIdStr := strconv.FormatUint(channelId, 10)
	ppm := strconv.FormatInt(feePerMil, 10)

	// direction: inbound or outbound
	fieldId := strconv.FormatUint(channelId, 10) + "_" + direction
	align := "margin-left: 1px"
	if direction == "inbound" {
		if !ln.HasInboundFees() {
			return `<td style="width: 1ch; padding: 0px;""></td>`
		}
		align = "text-align: right"
	}

	if ln.AutoFeeEnabledAll && ln.AutoFeeEnabled[channelId] {
		align = "text-align: center"
	}

	nextPage := "/?"
	if showAll {
		nextPage += "showall&"
	}

	t := `<td title="` + strings.Title(direction) + ` fee PPM" id="scramble" style="width: 6ch; padding: 0px; ` + align + `">`
	// for autofees show link
	if ln.AutoFeeEnabledAll && ln.AutoFeeEnabled[channelId] {
		rates, custom := ln.AutoFeeRatesSummary(channelId)
		if custom {
			rates = "*" + rates
		}

		change := "&nbsp"
		feeLog := ln.LastAutoFeeLog(channelId, direction == "inbound")
		if feeLog != nil {
			rates += "\nLast update " + timePassedAgo(time.Unix(feeLog.TimeStamp, 0))
			rates += "\nFrom " + formatSigned(int64(feeLog.OldRate))
			rates += " to " + formatSigned(int64(feeLog.NewRate))
			if feeLog.TimeStamp > time.Now().Add(-24*time.Hour).Unix() {
				if feeLog.NewRate > feeLog.OldRate {
					if config.Config.ColorScheme == "dark" {
						change = `<span style="color: lightgreen">⬆</span>`
					} else {
						change = `<span style="color: green">⬆</span>`
					}

				} else {
					change = `<span style="color: red">⬇</span>`
				}
			}
		}

		t += "<a title=\"" + strings.Title(direction) + " fee PPM\nAuto Fees enabled\nRule: " + rates + "\" href=\"/af?id=" + channelIdStr + "\">" + formatSigned(feePerMil) + "</a>" + change
	} else {
		t += `<form id="` + fieldId + `" autocomplete="off" action="/submit" method="post">`
		t += `<input autocomplete="false" name="hidden" type="text" style="display:none;">`
		t += `<input type="hidden" name="action" value="setFee">`
		t += `<input type="hidden" name="peerNodeId" value="` + peerNodeId + `">`
		t += `<input type="hidden" name="direction" value="` + direction + `">`
		t += `<input type="hidden" name="nextPage" value="` + nextPage + `">`
		t += `<input type="hidden" name="channelId" value="` + channelIdStr + `">`
		t += `<input type="number" style="width: 6ch; text-align: center; background-color: ` + backgroundColor + `; color: ` + fontColor + `" name="feeRate" value="` + ppm + `" onchange="feeSubmitForm('` + fieldId + `')">`
		t += `</form>`
	}

	t += `</td>`

	return t
}

// Template function to check if the element is the last one in the slice
func last(x int, a interface{}) bool {
	return x == len(*(a.(*[]ln.DataPoint)))-1
}

func advertiseBalances() {

	client, cleanup, err := ps.GetClient(config.Config.RpcHost)
	if err != nil {
		return
	}
	defer cleanup()

	res2, err := ps.LiquidGetBalance(client)
	if err != nil {
		return
	}

	res3, err := ps.ListPeers(client)
	if err != nil {
		return
	}

	cl, clean, er := ln.GetClient()
	if er != nil {
		return
	}
	defer clean()

	// store for replies to poll requests
	ln.LiquidBalance = res2.GetSatAmount()
	ln.BitcoinBalance = uint64(ln.ConfirmedWalletBalance(cl))

	cutOff := time.Now().AddDate(0, 0, -1).Unix() - 120

	for _, peer := range res3.GetPeers() {
		if ln.Implementation == "LND" {
			// refresh balances received over 24 hours ago + 2 minutes ago
			pollPeer := false
			if ptr := ln.LiquidBalances[peer.NodeId]; ptr != nil {
				if ptr.TimeStamp < cutOff {
					pollPeer = true
					// delete stale information
					ln.LiquidBalances[peer.NodeId] = nil
				}
			}
			if ptr := ln.BitcoinBalances[peer.NodeId]; ptr != nil {
				if ptr.TimeStamp < cutOff {
					pollPeer = true
					// delete stale information
					ln.BitcoinBalances[peer.NodeId] = nil
				}
			}

			if pollPeer {
				ln.SendCustomMessage(cl, peer.NodeId, &ln.Message{
					Version: ln.MessageVersion,
					Memo:    "poll",
				})
			}
		}

		if ln.AdvertiseLiquidBalance {
			ptr := ln.SentLiquidBalances[peer.NodeId]
			if ptr != nil {
				if ptr.Amount == ln.LiquidBalance && ptr.TimeStamp > time.Now().AddDate(0, 0, -1).Unix() {
					// do not resend within 24 hours unless changed
					continue
				}
			}

			if ln.SendCustomMessage(cl, peer.NodeId, &ln.Message{
				Version: ln.MessageVersion,
				Memo:    "balance",
				Asset:   "lbtc",
				Amount:  ln.LiquidBalance,
			}) == nil {
				// save announcement details
				if ptr == nil {
					ln.SentLiquidBalances[peer.NodeId] = new(ln.BalanceInfo)
				}
				ln.SentLiquidBalances[peer.NodeId].Amount = ln.LiquidBalance
				ln.SentLiquidBalances[peer.NodeId].TimeStamp = time.Now().Unix()
			}
		}

		if ln.AdvertiseBitcoinBalance {
			ptr := ln.SentBitcoinBalances[peer.NodeId]
			if ptr != nil {
				if ptr.Amount == ln.BitcoinBalance && ptr.TimeStamp > time.Now().AddDate(0, 0, -1).Unix() {
					// do not resend within 24 hours unless changed
					continue
				}
			}

			if ln.SendCustomMessage(cl, peer.NodeId, &ln.Message{
				Version: ln.MessageVersion,
				Memo:    "balance",
				Asset:   "btc",
				Amount:  ln.BitcoinBalance,
			}) == nil {
				// save announcement details
				if ptr == nil {
					ln.SentBitcoinBalances[peer.NodeId] = new(ln.BalanceInfo)
				}
				ln.SentBitcoinBalances[peer.NodeId].Amount = ln.BitcoinBalance
				ln.SentBitcoinBalances[peer.NodeId].TimeStamp = time.Now().Unix()
			}
		}
	}
}

func pollBalances() {

	if initalPollComplete || ln.Implementation != "LND" {
		return
	}

	client, cleanup, err := ps.GetClient(config.Config.RpcHost)
	if err != nil {
		return
	}
	defer cleanup()

	res, err := ps.ListPeers(client)
	if err != nil {
		return
	}

	cl, clean, er := ln.GetClient()
	if er != nil {
		return
	}
	defer clean()

	for _, peer := range res.GetPeers() {
		if err := ln.SendCustomMessage(cl, peer.NodeId, &ln.Message{
			Version: ln.MessageVersion,
			Memo:    "poll",
		}); err != nil {
			log.Println("Failed to poll balances from", getNodeAlias(peer.NodeId), err)
		} else {
			initalPollComplete = true
		}
	}

	if initalPollComplete {
		log.Println("Polled peers for balances")
	}
}
