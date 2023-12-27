package main

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"

	"github.com/elementsproject/peerswap/peerswaprpc"
	"google.golang.org/grpc"
)

type Page struct {
	Title     string
	SatAmount string
}

func main() {
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))
	http.HandleFunc("/", indexPage)

	log.Println("Listening on http://localhost:8088")
	err := http.ListenAndServe(":8088", nil)

	if err != nil {
		log.Fatal(err)
	}
}

func indexPage(w http.ResponseWriter, r *http.Request) {

	host := "localhost:42069"

	maxMsgRecvSize := grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200)
	opts := []grpc.DialOption{
		grpc.WithDefaultCallOptions(maxMsgRecvSize),
		grpc.WithInsecure(),
	}

	conn, err := grpc.Dial(host, opts...)
	if err != nil {
		log.Fatal(fmt.Errorf("unable to connect to RPC server: %v",
			err))
	}

	defer func() { conn.Close() }()

	psClient := peerswaprpc.NewPeerSwapClient(conn)

	ctx := context.Background()
	// make the request to get liquid balance
	resp, err := psClient.LiquidGetBalance(ctx, &peerswaprpc.GetBalanceRequest{})
	if err != nil {
		log.Fatal(err)
	}

	satAmount := resp.GetSatAmount()

	fmt.Println(satAmount)

	tmpl, err := template.ParseFiles("cmd/web/layout.html")
	if err != nil {
		// Log the detailed error
		log.Println(err.Error())
		// Return a generic "Internal Server Error" message
		http.Error(w, http.StatusText(500), 500)
		return
	}

	data := Page{
		Title:     "PeerSwap",
		SatAmount: strconv.FormatUint(satAmount, 10),
	}

	err = tmpl.Execute(w, data)
	if err != nil {
		log.Fatalln(err.Error())
		http.Error(w, http.StatusText(500), 500)
	}
}
