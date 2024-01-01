package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"text/template"
	"time"

	"github.com/elementsproject/peerswap/peerswaprpc"
	"github.com/gorilla/mux"

	"peerswap-web/utils"
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
)

const version = "1.0.0"

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
	utils.LoadConfig(*configFile)

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
	r.HandleFunc("/save", saveHandler)
	r.HandleFunc("/config", configHandler)
	r.HandleFunc("/stop", stopHandler)
	r.HandleFunc("/update", updateHandler)
	r.HandleFunc("/liquid", liquidHandler)

	if *configFile != "" {
		// wait a little in case it was an autorestart to avoid port used error
		time.Sleep(2 * time.Second)
	}

	// Start the server
	http.Handle("/", r)

	log.Println("Listening on http://localhost:" + utils.Config.ListenPort)
	err = http.ListenAndServe(":"+utils.Config.ListenPort, nil)
	if err != nil {
		log.Fatal(err)
	}
}

func indexHandler(w http.ResponseWriter, r *http.Request) {

	host := utils.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := utils.GetClient(host)
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
	suspiciousPeers := res4.GetSuspiciousPeerList()

	//check for error message to display
	message := ""
	keys, ok := r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		message = keys[0]
	}

	type Page struct {
		AllowSwapRequests bool
		Message           string
		ColorScheme       string
		SatAmount         string
		ListPeers         string
		ListSwaps         string
	}

	data := Page{
		AllowSwapRequests: utils.Config.AllowSwapRequests,
		Message:           message,
		ColorScheme:       utils.Config.ColorScheme,
		SatAmount:         utils.FormatWithThousandSeparators(satAmount),
		ListPeers:         utils.ConvertPeersToHTMLTable(peers, allowlistedPeers, suspiciousPeers),
		ListSwaps:         utils.ConvertSwapsToHTMLTable(swaps),
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
	host := utils.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := utils.GetClient(host)
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
	peer := utils.FindPeerById(peers, id)

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
	suspiciousPeers := res2.GetSuspiciousPeerList()

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
		NodeUrl     string
		Allowed     bool
		Suspicious  bool
		LBTC        bool
		BTC         bool
		SatAmount   string
	}

	data := Page{
		Message:     message,
		ColorScheme: utils.Config.ColorScheme,
		Peer:        peer,
		PeerAlias:   utils.GetNodeAlias(peer.NodeId),
		NodeUrl:     utils.Config.MempoolApi + "/lightning/node/",
		Allowed:     utils.StringIsInSlice(peer.NodeId, allowlistedPeers),
		Suspicious:  utils.StringIsInSlice(peer.NodeId, suspiciousPeers),
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
	}

	data := Page{
		ColorScheme: utils.Config.ColorScheme,
		Id:          id,
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
	host := utils.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := utils.GetClient(host)
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

	url := utils.Config.MempoolApi + "/tx/"
	if swap.Asset == "lbtc" {
		url = utils.Config.LiquidApi + "/tx/"
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
	swapData += utils.VisualiseSwapStatus(swap.State, true)
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
	swapData += utils.GetNodeAlias(swap.InitiatorNodeId)
	swapData += `&nbsp<a href="`
	swapData += utils.Config.MempoolApi + "/lightning/node/" + swap.InitiatorNodeId
	swapData += `" target="_blank">🔗</a></td></tr>
			<tr><td style="text-align: right">Peer:</td><td style="overflow-wrap: break-word;">`
	swapData += utils.GetNodeAlias(swap.PeerNodeId)
	swapData += `&nbsp<a href="`
	swapData += utils.Config.MempoolApi + "/lightning/node/" + swap.PeerNodeId
	swapData += `" target="_blank">🔗</a></td></tr>
			<tr><td style="text-align: right">Amount:</td><td>`
	swapData += utils.FormatWithThousandSeparators(swap.Amount)
	swapData += `</td></tr>
			<tr><td style="text-align: right">ChannelId:</td><td>`
	swapData += swap.ChannelId
	swapData += `</td></tr>
			<tr><td style="text-align: right">OpeningTxId:</td><td style="overflow-wrap: break-word;">`
	swapData += swap.OpeningTxId
	swapData += `&nbsp<a href="`
	swapData += url + swap.OpeningTxId
	swapData += `" target="_blank">🔗</a>`
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
		Config      utils.Configuration
	}

	data := Page{
		Message:     message,
		ColorScheme: utils.Config.ColorScheme,
		Config:      utils.Config,
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

	host := utils.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := utils.GetClient(host)
	if err != nil {
		log.Printf("unable to connect to RPC server: %v", err)
		redirectWithError(w, r, "/liquid?", err)
		return
	}
	defer cleanup()

	res, err := client.LiquidGetAddress(ctx, &peerswaprpc.GetAddressRequest{})
	if err != nil {
		log.Printf("unable to connect to RPC server: %v", err)
		redirectWithError(w, r, "/liquid?", err)
		return
	}

	res2, err := client.LiquidGetBalance(ctx, &peerswaprpc.GetBalanceRequest{})
	if err != nil {
		log.Printf("unable to connect to RPC server: %v", err)
		redirectWithError(w, r, "/liquid?", err)
		return
	}

	type Page struct {
		Message       string
		ColorScheme   string
		LiquidAddress string
		SatAmount     string
		TxId          string
		LiquidUrl     string
	}

	data := Page{
		Message:       message,
		ColorScheme:   utils.Config.ColorScheme,
		LiquidAddress: res.Address,
		SatAmount:     utils.FormatWithThousandSeparators(res2.GetSatAmount()),
		TxId:          txid,
		LiquidUrl:     utils.Config.LiquidApi + "/tx/" + txid,
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
		host := utils.Config.RpcHost
		ctx := context.Background()

		client, cleanup, err := utils.GetClient(host)
		if err != nil {
			redirectWithError(w, r, "/config?", err)
			return
		}
		defer cleanup()

		switch action {
		case "sendLiquid":
			amt, err := strconv.ParseUint(r.FormValue("sendAmount"), 10, 64)
			if err != nil {
				redirectWithError(w, r, "/liquid?", err)
				return
			}
			sendResult, err := client.LiquidSendToAddress(ctx, &peerswaprpc.SendToAddressRequest{
				Address:   r.FormValue("sendAddress"),
				SatAmount: amt,
			})
			if err != nil {
				redirectWithError(w, r, "/liquid?", err)
				return
			}
			// Redirect to liquid page with TxId
			http.Redirect(w, r, "/liquid?msg=\"\"&txid="+sendResult.TxId, http.StatusSeeOther)
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

// saves config
func saveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// Parse the form data
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Error parsing form data", http.StatusBadRequest)
			return
		}

		mustRestart := utils.Config.ListenPort != r.FormValue("listenPort")
		allowSwapRequests, err := strconv.ParseBool(r.FormValue("allowSwapRequests"))
		if err != nil {
			redirectWithError(w, r, "/config?", err)
			return
		}

		if allowSwapRequests != utils.Config.AllowSwapRequests {
			err = utils.AllowSwapRequests(allowSwapRequests)
			if err != nil {
				redirectWithError(w, r, "/config?", err)
				return
			}
		}

		utils.Config.AllowSwapRequests = allowSwapRequests
		utils.Config.RpcHost = r.FormValue("rpcHost")
		utils.Config.ListenPort = r.FormValue("listenPort")
		utils.Config.ColorScheme = r.FormValue("colorScheme")
		utils.Config.MempoolApi = r.FormValue("mempoolApi")
		utils.Config.LiquidApi = r.FormValue("liquidApi")
		utils.Config.ConfigFile = r.FormValue("configFile")

		mh, err := strconv.ParseUint(r.FormValue("maxHistory"), 10, 16)
		if err != nil {
			redirectWithError(w, r, "/config?", err)
			return
		}
		utils.Config.MaxHistory = uint(mh)

		if err = utils.SaveConfig(); err != nil {
			redirectWithError(w, r, "/config?", err)
			return
		}

		if mustRestart {
			// Execute a new instance of the program passing --configfile as a parameter
			cmd := exec.Command(os.Args[0], "--configfile="+utils.Config.ConfigFile)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			if err := cmd.Start(); err != nil {
				fmt.Println("Failed to restart:", err)
				return
			}

			http.Error(w, "PeerSwap Web has restarted. New url: http://localhost:"+utils.Config.ListenPort, http.StatusBadGateway)
			fmt.Println("Restarted successfully! PID: ", cmd.Process.Pid)
			go func() {
				time.Sleep(3 * time.Second) // Delay for 3 seconds
				os.Exit(0)                  // Exit the program
			}()
		}
		if err != nil {
			redirectWithError(w, r, "/?", err)
		} else {
			http.Redirect(w, r, "/", http.StatusSeeOther)
		}
		return
	} else {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func stopHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "PeerSwap Web has stopped.", http.StatusBadGateway)
	fmt.Println("Stop requested")
	go func() {
		time.Sleep(3 * time.Second) // Delay for 3 seconds
		os.Exit(0)                  // Exit the program
	}()
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
