package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"log"

	"github.com/elementsproject/peerswap/peerswaprpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

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
		//grpc.WithInsecure(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	return conn, nil
}

func stopPeerSwapd() {
	host := config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		return
	}
	defer cleanup()

	log.Println("Stopping peerswapd...")

	_, err = client.Stop(ctx, &peerswaprpc.Empty{})
	if err != nil {
		log.Printf("unable to stop peerswapd: %v", err)
	}
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
func convertPeersToHTMLTable(peers []*peerswaprpc.PeerSwapPeer, allowlistedPeers []string, suspiciousPeers []string) string {

	type Table struct {
		AvgLocal uint64
		HtmlBlob string
	}

	var unsortedTable []Table

	for _, peer := range peers {
		var totalLocal uint64
		var totalCapacity uint64

		table := "<table style=\"table-layout:fixed; width: 100%\">"
		table += "<tr style=\"border: 1px dotted\">"
		table += "<td id=\"scramble\" style=\"float: left; text-align: left; width: 70%;\">"

		// alias is a link to open peer details page
		table += "<a href=\"/peer?id=" + peer.NodeId + "\">"

		if stringIsInSlice(peer.NodeId, allowlistedPeers) {
			table += "✅&nbsp"
		} else {
			table += "⛔&nbsp"
		}

		if stringIsInSlice(peer.NodeId, suspiciousPeers) {
			table += "🔍&nbsp"
		}

		table += getNodeAlias(peer.NodeId)
		table += "</a>"
		table += "</td><td style=\"float: right; text-align: right; width:30%;\">"
		table += "<a href=\"/peer?id=" + peer.NodeId + "\">"

		if stringIsInSlice("lbtc", peer.SupportedAssets) {
			table += "🌊&nbsp"
		}
		if stringIsInSlice("btc", peer.SupportedAssets) {
			table += "₿&nbsp"
		}
		if peer.SwapsAllowed {
			table += "✅"
		} else {
			table += "⛔"
		}
		table += "</a>"
		table += "</td></tr></table>"

		table += "<table style=\"table-layout:fixed;\">"

		// Construct channels data
		for _, channel := range peer.Channels {

			// red background for inactive channels
			bc := "#590202"
			if config.ColorScheme == "light" {
				bc = "#fcb6b6"
			}

			if channel.Active {
				// green background for active channels
				bc = "#224725"
				if config.ColorScheme == "light" {
					bc = "#e6ffe8"
				}
			}

			table += "<tr style=\"background-color: " + bc + "\"; >"
			table += "<td id=\"scramble\" style=\"width: 250px; text-align: center\">"
			table += formatWithThousandSeparators(channel.LocalBalance)
			table += "</td><td style=\"width: 25%; text-align: center\">"
			local := channel.LocalBalance
			capacity := channel.LocalBalance + channel.RemoteBalance
			totalLocal += local
			totalCapacity += capacity
			table += "<a href=\"/peer?id=" + peer.NodeId + "\">"
			table += "<progress style=\"width: 100%;\" value=" + strconv.FormatUint(local, 10) + " max=" + strconv.FormatUint(capacity, 10) + ">1</progress>"
			table += "</a></td>"
			table += "<td id=\"scramble\" style=\"width: 250px; text-align: center\">"
			table += formatWithThousandSeparators(channel.RemoteBalance)
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

	if len(swaps) == 0 {
		return ""
	}

	type Table struct {
		TimeStamp int64
		HtmlBlob  string
	}
	var unsortedTable []Table

	for _, swap := range swaps {
		table := "<tr>"
		table += "<td style=\"width: 30%; text-align: left\">"

		tm := timePassedAgo(time.Unix(swap.CreatedAt, 0).UTC())

		// clicking on timestamp will open swap details page
		table += "<a href=\"/swap?id=" + swap.Id + "\">" + tm + "</a> "
		table += "</td><td style=\"text-align: left\">"
		table += visualiseSwapStatus(swap.State, false) + "&nbsp"
		table += formatWithThousandSeparators(swap.Amount)

		switch swap.Type + swap.Role {
		case "swap-outsender":
			table += " ⚡&nbsp⇨&nbsp"
		case "swap-insender":
			table += " ⚡&nbsp⇦&nbsp"
		case "swap-outreceiver":
			table += " ⚡&nbsp⇦&nbsp"
		case "swap-inreceiver":
			table += " ⚡&nbsp⇨&nbsp"
		}

		switch swap.Asset {
		case "lbtc":
			table += "🌊"
		case "btc":
			table += "₿"
		default:
			table += "?"
		}

		table += "</td><td id=\"scramble\" style=\"overflow-wrap: break-word;\">"

		switch swap.Role {
		case "receiver":
			table += " ⇦&nbsp"
		case "sender":
			table += " ⇨&nbsp"
		default:
			table += " ?&nbsp"
		}

		table += "<a href=\"/peer?id=" + swap.PeerNodeId + "\">"
		table += getNodeAlias(swap.PeerNodeId)
		table += "</a></td></tr>"

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
		if counter > config.MaxHistory {
			break
		}
		table += t.HtmlBlob
	}
	table += "</table>"
	return table
}
