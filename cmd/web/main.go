package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"text/template"

	"github.com/elementsproject/peerswap/peerswaprpc"
	"google.golang.org/grpc"
)

type Configuration struct {
	RpcHost     string
	ListenPort  string
	ColorScheme string
}

var Config Configuration
var templates = template.New("")

func main() {
	// simulate loading from config file
	Config = Configuration{
		RpcHost:     "localhost:42069",
		ListenPort:  "8088",
		ColorScheme: "dark", // dark or light
	}

	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))
	http.HandleFunc("/", homePage)

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
	//fmt.Println(peers)

	type Page struct {
		ColorScheme string
		SatAmount   string
		ListPeers   string
	}

	data := Page{
		ColorScheme: Config.ColorScheme,
		SatAmount:   addThousandSeparators(satAmount),
		ListPeers:   convertPeersToHTMLTable(peers),
	}

	// executing template named "homepage"
	err = templates.ExecuteTemplate(w, "homepage", data)
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

func getClientConn(address string) (*grpc.ClientConn,
	error) {

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

func addThousandSeparators(n uint64) string {
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

// converts a list of peers into an HTML table
func convertPeersToHTMLTable(peers []*peerswaprpc.PeerSwapPeer) string {
	table := ""

	for _, peer := range peers {
		table += "<p style=\"word-wrap: break-word\">"
		if peer.SwapsAllowed {
			table += "✅"
		} else {
			table += "⛔"
		}
		if stringInSlice("btc", peer.SupportedAssets) {
			table += "₿"
		}
		if stringInSlice("lbtc", peer.SupportedAssets) {
			table += "🌊"
		}

		table += getNodeAlias(peer.NodeId)
		table += "</p>"
		table += "<table align=center>"

		// Construct channels data
		for _, channel := range peer.Channels {
			table += "<tr><td style=\"width: 40%; text-align: center\">"
			table += addThousandSeparators(channel.LocalBalance)
			table += "</td><td>"
			local := strconv.FormatUint(channel.LocalBalance, 10)
			total := strconv.FormatUint(channel.LocalBalance+channel.RemoteBalance, 10)
			active := "❌"
			if channel.Active {
				active = "<progress value=" + local + " max=" + total + "></progress>"
			}
			table += active + "</td><td>"
			table += "<td style=\"width: 40%; text-align: center\">"
			table += addThousandSeparators(channel.RemoteBalance)
			table += "</td></tr>"
		}
		table += "</table>"
	}
	return table
}

func stringInSlice(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

func getNodeAlias(id string) string {
	// do to later
	return id
}
