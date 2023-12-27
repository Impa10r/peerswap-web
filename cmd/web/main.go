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
	RpcHost    string
	ListenPort string
}

var config Configuration

func main() {
	// simulate loading from config file
	config = Configuration{
		RpcHost:    "localhost:42069",
		ListenPort: "8088",
	}

	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))
	http.HandleFunc("/", homePage)

	log.Println("Listening on http://localhost:" + config.ListenPort)
	err := http.ListenAndServe(":"+config.ListenPort, nil)

	if err != nil {
		log.Fatal(err)
	}
}

func homePage(w http.ResponseWriter, r *http.Request) {

	host := config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		log.Fatal(fmt.Errorf("unable to connect to RPC server: %v", err))
	}
	defer cleanup()

	res, err := client.LiquidGetBalance(ctx, &peerswaprpc.GetBalanceRequest{})
	if err != nil {
		log.Fatal(err)
	}

	satAmount := res.GetSatAmount()

	fmt.Println(satAmount)

	// loading and parsing templates
	tmpl, err := template.New("").ParseGlob("templates/*.gohtml")
	if err != nil {
		// Log the detailed error
		log.Println(err.Error())
		// Return a generic "Internal Server Error" message
		http.Error(w, http.StatusText(500), 500)
		return
	}

	type Page struct {
		Title     string
		SatAmount string
	}

	data := Page{
		Title:     "PeerSwap",
		SatAmount: strconv.FormatUint(satAmount, 10),
	}

	// executing template named "homepage"
	err = tmpl.ExecuteTemplate(w, "homepage", data)
	if err != nil {
		log.Fatalln(err.Error())
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
