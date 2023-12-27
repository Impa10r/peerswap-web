package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"

	"github.com/go-openapi/strfmt"

	apiclient "peerswap-web/client"

	httptransport "github.com/go-openapi/runtime/client"
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

	RpcHost := os.Getenv("RPC_HOST")

	if RpcHost == "" {
		RpcHost = "localhost:42070"
	}
	// create the transport
	transport := httptransport.New(RpcHost, "", nil)

	// create the API client, with the transport
	client := apiclient.New(transport, strfmt.Default)

	// make the request to get liquid balance
	resp, err := client.PeerSwap.PeerSwapLiquidGetBalance(nil)
	if err != nil {
		log.Fatal(err)
	}

	satAmount := resp.Payload.SatAmount

	fmt.Printf("%#v\n", resp.Payload)

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
		SatAmount: satAmount,
	}

	err = tmpl.Execute(w, data)
	if err != nil {
		log.Fatalln(err.Error())
		http.Error(w, http.StatusText(500), 500)
	}
}
