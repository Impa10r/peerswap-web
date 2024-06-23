package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"peerswap-web/cmd/psweb/bitcoin"
	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/db"
	"peerswap-web/cmd/psweb/internet"
	"peerswap-web/cmd/psweb/liquid"
	"peerswap-web/cmd/psweb/ln"
	"peerswap-web/cmd/psweb/ps"

	"github.com/elementsproject/peerswap/peerswaprpc"
	"github.com/gorilla/sessions"
)

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
		Authenticated     bool
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
		Authenticated:     config.Config.SecureConnection && config.Config.Password != "",
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

	// executing template named "homepage" with retries
	executeTemplate(w, "homepage", data)
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
		// Search amoung all Lightning peers
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

	// get routing stats
	for _, ch := range peer.Channels {
		stat := ln.GetForwardingStats(ch.ChannelId)
		stats = append(stats, stat)

		info := ln.GetChannelInfo(cl, ch.ChannelId, peer.NodeId)
		info.LocalBalance = ch.GetLocalBalance()
		info.RemoteBalance = ch.GetRemoteBalance()
		info.Active = ch.GetActive()
		info.LocalPct = info.LocalBalance * 100 / info.Capacity
		channelInfo = append(channelInfo, info)

		sumLocal += ch.GetLocalBalance()
		sumRemote += ch.GetRemoteBalance()

		// should not be less than both Min HTLC setting
		keysendSats = max(keysendSats, info.PeerMinHtlc)
		keysendSats = max(keysendSats, info.OurMinHtlc)

		// add AF info
		if ln.AutoFeeEnabledAll && ln.AutoFeeEnabled[ch.ChannelId] {
			rates, custom := ln.AutoFeeRatesSummary(ch.ChannelId)
			if custom {
				rates = "*" + rates
			}

			feeLog := ln.LastAutoFeeLog(ch.ChannelId, false)
			if feeLog != nil {
				rates += ", last update " + timePassedAgo(time.Unix(feeLog.TimeStamp, 0))
				rates += " from " + formatWithThousandSeparators(uint64(feeLog.OldRate))
				rates += " to " + formatWithThousandSeparators(uint64(feeLog.NewRate))
			}

			info.AutoFeeLog = "<a href=\"/af?id=" + strconv.FormatUint(ch.ChannelId, 10) + "\">AF rule</a>: " + rates
		}
	}

	//check for error errorMessage to display
	errorMessage := ""
	keys, ok = r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		errorMessage = keys[0]
	}

	//check for pop-up message to display
	popupMessage := ""
	keys, ok = r.URL.Query()["msg"]
	if ok && len(keys[0]) > 0 {
		popupMessage = keys[0]
	}

	type Page struct {
		Authenticated     bool
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
		HasInboundFees    bool
	}

	feeRate := liquid.EstimateFee()
	if !psPeer {
		feeRate = mempoolFeeRate
	}

	// to be conservative
	bitcoinFeeRate := max(ln.EstimateFee(), mempoolFeeRate)

	data := Page{
		Authenticated:     config.Config.SecureConnection && config.Config.Password != "",
		ErrorMessage:      errorMessage,
		PopUpMessage:      popupMessage,
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
		HasInboundFees:    ln.HasInboundFees(),
	}

	// executing template named "peer"
	executeTemplate(w, "peer", data)
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
		Authenticated    bool
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
		Authenticated:    config.Config.SecureConnection && config.Config.Password != "",
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
	executeTemplate(w, "bitcoin", data)
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

func afHandler(w http.ResponseWriter, r *http.Request) {
	channelId := uint64(0)
	peerName := "Default Rule"

	keys, ok := r.URL.Query()["id"]
	if ok && len(keys) == 1 {
		id, err := strconv.ParseUint(keys[0], 10, 64)
		if err == nil {
			channelId = id
		}
	}

	rule, isCustom := ln.AutoFeeRule(channelId)

	// Get Lightning client
	cl, clean, er := ln.GetClient()
	if er != nil {
		redirectWithError(w, r, "/config?", er)
		return
	}
	defer clean()

	var channelList []*ln.AutoFeeStatus
	anyEnabled := false
	peerId := ""

	// Get all public Lightning channels
	res, err := ln.ListPeers(cl, "", nil)
	if err != nil {
		redirectWithError(w, r, "/?", err)
		return
	}

	// get fee rates for all channels
	outboundFeeRates := make(map[uint64]int64)
	inboundFeeRates := make(map[uint64]int64)

	ln.FeeReport(cl, outboundFeeRates, inboundFeeRates)

	capacity := uint64(0)
	localPct := uint64(0)
	feeRate := outboundFeeRates[channelId]
	inboundRate := inboundFeeRates[channelId]

	for _, peer := range res.GetPeers() {
		alias := getNodeAlias(peer.NodeId)
		for _, ch := range peer.Channels {
			rule, custom := ln.AutoFeeRatesSummary(ch.ChannelId)
			af, _ := ln.AutoFeeRule(ch.ChannelId)
			channelList = append(channelList, &ln.AutoFeeStatus{
				Enabled:     ln.AutoFeeEnabled[ch.ChannelId],
				Capacity:    ch.LocalBalance + ch.RemoteBalance,
				Alias:       alias,
				LocalPct:    ch.LocalBalance * 100 / (ch.LocalBalance + ch.RemoteBalance),
				Rule:        rule,
				Custom:      custom,
				AutoFee:     af,
				FeeRate:     outboundFeeRates[ch.ChannelId],
				InboundRate: inboundFeeRates[ch.ChannelId],
				ChannelId:   ch.ChannelId,
			})

			if ch.ChannelId == channelId {
				peerName = alias
				peerId = peer.NodeId
				capacity = ch.LocalBalance + ch.RemoteBalance
				localPct = ch.LocalBalance * 100 / (ch.LocalBalance + ch.RemoteBalance)
			}
			if ln.AutoFeeEnabled[ch.ChannelId] {
				anyEnabled = true
			}
		}
	}

	// sort by LocalPct descending
	sort.Slice(channelList, func(i, j int) bool {
		return channelList[i].LocalPct < channelList[j].LocalPct
	})
	//check for error errorMessage to display
	errorMessage := ""
	keys, ok = r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		errorMessage = keys[0]
	}

	//check for pop-up message to display
	popupMessage := ""
	keys, ok = r.URL.Query()["msg"]
	if ok && len(keys[0]) > 0 {
		popupMessage = keys[0]
	}

	chart := ln.PlotPPM(channelId)
	// bubble square area reflects amount
	for i, p := range *chart {
		(*chart)[i].R = uint64(math.Sqrt(float64(p.Amount) / 10_000))
		(*chart)[i].Label = "Routed: " + formatWithThousandSeparators(p.Amount) + ", Fee: " + formatWithThousandSeparators(p.Fee) + ", PPM: " + formatWithThousandSeparators(p.PPM)
	}

	type Page struct {
		Authenticated  bool
		ErrorMessage   string
		PopUpMessage   string
		MempoolFeeRate float64
		ColorScheme    string
		ChannelId      uint64
		PeerName       string
		PeerId         string
		Capacity       uint64
		LocalPct       uint64
		FeeRate        int64
		InboundRate    int64
		GlobalEnabled  bool
		ChannelList    []*ln.AutoFeeStatus
		Params         *ln.AutoFeeParams
		CustomRule     bool
		Enabled        bool // for the displayed channel
		AnyEnabled     bool // for any channel
		HasInboundFees bool
		Chart          *[]ln.DataPoint
	}

	data := Page{
		Authenticated:  config.Config.SecureConnection && config.Config.Password != "",
		ErrorMessage:   errorMessage,
		PopUpMessage:   popupMessage,
		MempoolFeeRate: mempoolFeeRate,
		ColorScheme:    config.Config.ColorScheme,
		GlobalEnabled:  ln.AutoFeeEnabledAll,
		PeerName:       peerName,
		PeerId:         peerId,
		Capacity:       capacity,
		LocalPct:       localPct,
		FeeRate:        feeRate,
		InboundRate:    inboundRate,
		ChannelId:      channelId,
		ChannelList:    channelList,
		Params:         rule,
		CustomRule:     isCustom,
		Enabled:        ln.AutoFeeEnabled[channelId],
		AnyEnabled:     anyEnabled,
		HasInboundFees: ln.HasInboundFees(),
		Chart:          chart,
	}

	// executing template named "af"
	executeTemplate(w, "af", data)
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
		Authenticated  bool
		ColorScheme    string
		Id             string
		ErrorMessage   string
		PopUpMessage   string
		MempoolFeeRate float64
		IsPending      bool
	}

	data := Page{
		Authenticated:  config.Config.SecureConnection && config.Config.Password != "",
		ColorScheme:    config.Config.ColorScheme,
		Id:             id,
		ErrorMessage:   "",
		PopUpMessage:   "",
		MempoolFeeRate: mempoolFeeRate,
		IsPending:      isPending,
	}

	// executing template named "swap"
	executeTemplate(w, "swap", data)
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
		<div class="box has-text-left">
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
		Authenticated   bool
		ErrorMessage    string
		PopUpMessage    string
		MempoolFeeRate  float64
		ColorScheme     string
		Config          config.Configuration
		Version         string
		Latest          string
		Implementation  string
		HTTPS           string
		IsPossibleHTTPS bool // disabled on Umbrel
	}

	data := Page{
		Authenticated:   config.Config.SecureConnection && config.Config.Password != "",
		ErrorMessage:    errorMessage,
		PopUpMessage:    "",
		MempoolFeeRate:  mempoolFeeRate,
		ColorScheme:     config.Config.ColorScheme,
		Config:          config.Config,
		Version:         version,
		Latest:          latestVersion,
		Implementation:  ln.Implementation,
		HTTPS:           "https://" + hostname + ".local:" + config.Config.SecurePort,
		IsPossibleHTTPS: os.Getenv("NO_HTTPS") == "",
	}

	// executing template named "config"
	executeTemplate(w, "config", data)
}

func caHandler(w http.ResponseWriter, r *http.Request) {
	//check for error message to display
	errorMessage := ""
	keys, ok := r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		errorMessage = keys[0]
	}

	password, err := config.GeneratePassword(10)
	if err != nil {
		log.Println("GeneratePassword:", err)
		redirectWithError(w, r, "/config?", err)
		return
	}

	type Page struct {
		Authenticated  bool
		ErrorMessage   string
		PopUpMessage   string
		MempoolFeeRate float64
		ColorScheme    string
		Config         config.Configuration
		Password       string
	}

	data := Page{
		Authenticated:  config.Config.SecureConnection && config.Config.Password != "",
		ErrorMessage:   errorMessage,
		PopUpMessage:   "",
		MempoolFeeRate: mempoolFeeRate,
		ColorScheme:    config.Config.ColorScheme,
		Config:         config.Config,
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
	executeTemplate(w, "ca", data)
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		r.ParseForm()
		if r.FormValue("password") == config.Config.Password {
			session, _ := store.Get(r, "session")
			session.Options = &sessions.Options{
				Path:   "/",
				MaxAge: 604800, // 7 days
			}
			session.Values["authenticated"] = true
			session.Save(r, w)
			http.Redirect(w, r, "/", http.StatusFound)
		} else {
			// delay brute force
			time.Sleep(5 * time.Second)
			redirectWithError(w, r, "/login?", errors.New("invalid password"))
		}
	} else {
		//check for error message to display
		errorMessage := ""
		keys, ok := r.URL.Query()["err"]
		if ok && len(keys[0]) > 0 {
			errorMessage = keys[0]
		}

		type Page struct {
			Authenticated  bool
			ErrorMessage   string
			PopUpMessage   string
			MempoolFeeRate float64
			ColorScheme    string
			Config         config.Configuration
		}

		data := Page{
			Authenticated:  config.Config.SecureConnection && config.Config.Password != "",
			ErrorMessage:   errorMessage,
			PopUpMessage:   "",
			MempoolFeeRate: mempoolFeeRate,
			ColorScheme:    config.Config.ColorScheme,
			Config:         config.Config,
		}

		// executing template named "login"
		executeTemplate(w, "login", data)
	}
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := store.Get(r, "session")
	session.Values["authenticated"] = false
	session.Options.MaxAge = -1 // MaxAge < 0 means delete the cookie immediately.
	session.Save(r, w)
	http.Redirect(w, r, "/login", http.StatusFound)
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
		Authenticated           bool
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
		Authenticated:           config.Config.SecureConnection && config.Config.Password != "",
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
	executeTemplate(w, "liquid", data)
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
		case "saveAutoFee":
			channelId, err := strconv.ParseUint(r.FormValue("channelId"), 10, 64)
			if err != nil {
				redirectWithError(w, r, "/af?", err)
				return
			}

			rule := &ln.AutoFeeDefaults
			msg := ""

			if r.FormValue("update_button") != "" {
				// channelId == 0 means default rule
				msg = "Default rule updated"

				if channelId > 0 {
					// custom rule
					msg = "Custom rule updated"
					if ln.AutoFee[channelId] == nil {
						// add new
						ln.AutoFee[channelId] = new(ln.AutoFeeParams)
						msg = "Custom rule added"
					}
					rule = ln.AutoFee[channelId]
				}

				rule.FailedBumpPPM, err = strconv.Atoi(r.FormValue("failBump"))
				if err != nil {
					redirectWithError(w, r, "/af?", err)
					return
				}

				rule.FailedMoveThreshold, err = strconv.Atoi(r.FormValue("failedMoveThreshold"))
				if err != nil {
					redirectWithError(w, r, "/af?", err)
					return
				}

				rule.LowLiqPct, err = strconv.Atoi(r.FormValue("lowLiqPct"))
				if err != nil {
					redirectWithError(w, r, "/af?", err)
					return
				}

				rule.LowLiqRate, err = strconv.Atoi(r.FormValue("lowLiqRate"))
				if err != nil {
					redirectWithError(w, r, "/af?", err)
					return
				}

				rule.NormalRate, err = strconv.Atoi(r.FormValue("normalRate"))
				if err != nil {
					redirectWithError(w, r, "/af?", err)
					return
				}

				rule.ExcessPct, err = strconv.Atoi(r.FormValue("excessPct"))
				if err != nil {
					redirectWithError(w, r, "/af?", err)
					return
				}

				rule.ExcessRate, err = strconv.Atoi(r.FormValue("excessRate"))
				if err != nil {
					redirectWithError(w, r, "/af?", err)
					return
				}

				rule.InactivityDays, err = strconv.Atoi(r.FormValue("inactivityDays"))
				if err != nil {
					redirectWithError(w, r, "/af?", err)
					return
				}

				rule.InactivityDropPPM, err = strconv.Atoi(r.FormValue("inactivityDropPPM"))
				if err != nil {
					redirectWithError(w, r, "/af?", err)
					return
				}

				rule.InactivityDropPct, err = strconv.Atoi(r.FormValue("inactivityDropPct"))
				if err != nil {
					redirectWithError(w, r, "/af?", err)
					return
				}

				rule.CoolOffHours, err = strconv.Atoi(r.FormValue("coolOffHours"))
				if err != nil {
					redirectWithError(w, r, "/af?", err)
					return
				}

				if ln.HasInboundFees() {
					rule.LowLiqDiscount, err = strconv.Atoi(r.FormValue("lowLiqDiscount"))
					if err != nil {
						redirectWithError(w, r, "/af?", err)
						return
					}
				}

				// persist to db
				if channelId > 0 {
					db.Save("AutoFees", "AutoFee", ln.AutoFee)
				} else {
					db.Save("AutoFees", "AutoFeeDefaults", ln.AutoFeeDefaults)
				}
			} else if r.FormValue("delete_button") != "" {
				if ln.AutoFee[channelId] != nil {
					// delete custom rule
					ln.AutoFee[channelId] = nil
					msg = "Custom rule deleted"
					// persist to db
					db.Save("AutoFees", "AutoFee", ln.AutoFee)
				}
			}

			// all done, display confirmation
			http.Redirect(w, r, "/af?id="+r.FormValue("channelId")+"&msg="+msg, http.StatusSeeOther)
			return

		case "toggleAutoFee":
			channelId, err := strconv.ParseInt(r.FormValue("channelId"), 10, 64)
			if err != nil {
				redirectWithError(w, r, "/af?", err)
				return
			}

			isEnabled := r.FormValue("enabled") == "on"

			cl, clean, er := ln.GetClient()
			if er != nil {
				redirectWithError(w, r, "/config?", er)
				return
			}
			defer clean()

			// Get all public Lightning channels
			res, err := ln.ListPeers(cl, "", nil)
			if err != nil {
				redirectWithError(w, r, "/?", err)
				return
			}

			msg := ""
			if channelId == 0 {
				// global setting
				ln.AutoFeeEnabledAll = isEnabled
				db.Save("AutoFees", "AutoFeeEnabledAll", ln.AutoFeeEnabledAll)
				msg = "Global AutoFees "
			} else if channelId == -1 {
				// toggle for all channels
				for _, peer := range res.GetPeers() {
					for _, ch := range peer.Channels {
						ln.AutoFeeEnabled[ch.ChannelId] = isEnabled
					}
				}
				db.Save("AutoFees", "AutoFeeEnabled", ln.AutoFeeEnabled)
				msg = "All per-channel AutoFees "

			} else {
				// toggle for a single channel
				ln.AutoFeeEnabled[uint64(channelId)] = isEnabled
				db.Save("AutoFees", "AutoFeeEnabled", ln.AutoFeeEnabled)

			outerLoop:
				for _, peer := range res.GetPeers() {
					for _, ch := range peer.Channels {
						if ch.ChannelId == uint64(channelId) {
							msg = "AutoFees for " + getNodeAlias(peer.NodeId) + " "
							break outerLoop
						}
					}
				}
			}

			if isEnabled {
				msg += "Enabled"
			} else {
				msg += "Disabled"
			}

			// all done, display confirmation
			http.Redirect(w, r, "/af?id="+r.FormValue("nextId")+"&msg="+msg, http.StatusSeeOther)
			return

		case "setFee":
			nextPage := r.FormValue("nextPage")

			feeRate, err := strconv.ParseInt(r.FormValue("feeRate"), 10, 64)
			if err != nil {
				redirectWithError(w, r, nextPage, err)
				return
			}

			channelId, err := strconv.ParseUint(r.FormValue("channelId"), 10, 64)
			if err != nil {
				redirectWithError(w, r, nextPage, err)
				return
			}

			inbound := r.FormValue("direction") == "inbound"

			if inbound {
				if !ln.HasInboundFees() {
					// CLN and LND < 0.18 cannot set inbound fees
					redirectWithError(w, r, nextPage, errors.New("inbound fees are not allowed by your LN backend"))
					return
				}

				if feeRate > 0 {
					// Only discounts are allowed for now
					redirectWithError(w, r, nextPage, errors.New("inbound fee rate cannot be positive"))
					return
				}
			} else {
				if feeRate < 0 {
					redirectWithError(w, r, nextPage, errors.New("outbound fee rate cannot be negative"))
					return
				}
			}

			oldRate, err := ln.SetFeeRate(r.FormValue("peerNodeId"), channelId, feeRate, inbound, false)
			if err != nil {
				redirectWithError(w, r, nextPage, err)
				return
			}

			// log change
			ln.LogFee(channelId, oldRate, int(feeRate), inbound, true)

			// all good, display confirmation
			msg := strings.Title(r.FormValue("direction")) + " fee rate updated to " + formatSigned(feeRate)
			http.Redirect(w, r, nextPage+"msg="+msg, http.StatusSeeOther)
			return

		case "setBase":
			nextPage := r.FormValue("nextPage")

			feeBase, err := strconv.ParseInt(r.FormValue("feeBase"), 10, 64)
			if err != nil {
				redirectWithError(w, r, nextPage, err)
				return
			}

			channelId, err := strconv.ParseUint(r.FormValue("channelId"), 10, 64)
			if err != nil {
				redirectWithError(w, r, nextPage, err)
				return
			}

			inbound := r.FormValue("direction") == "inbound"

			if inbound {
				if ln.Implementation == "CLN" || !ln.CanRBF() {
					// CLN and LND < 0.18 cannot set inbound fees
					redirectWithError(w, r, nextPage, errors.New("inbound fees are not allowed by your LN backend"))
					return
				}

				if feeBase > 0 {
					// Only discounts are allowed for now
					redirectWithError(w, r, nextPage, errors.New("inbound fee base cannot be positive"))
					return
				}
			} else {
				if feeBase < 0 {
					redirectWithError(w, r, nextPage, errors.New("outbound fee base cannot be negative"))
					return
				}
			}

			_, err = ln.SetFeeRate(r.FormValue("peerNodeId"), channelId, feeBase, inbound, true)
			if err != nil {
				redirectWithError(w, r, nextPage, err)
				return
			}

			// all good, display confirmation
			msg := strings.Title(r.FormValue("direction")) + " fee base updated to " + formatSigned(feeBase)
			http.Redirect(w, r, nextPage+"msg="+msg, http.StatusSeeOther)
			return

		case "setHtlcSize":
			nextPage := r.FormValue("nextPage")

			size, err := strconv.ParseInt(r.FormValue("size"), 10, 64)
			if err != nil {
				redirectWithError(w, r, nextPage, err)
				return
			}

			channelId, err := strconv.ParseUint(r.FormValue("channelId"), 10, 64)
			if err != nil {
				redirectWithError(w, r, nextPage, err)
				return
			}

			isMax := r.FormValue("minMax") == "max"

			err = ln.SetHtlcSize(r.FormValue("peerNodeId"), channelId, size*1000, isMax)
			if err != nil {
				redirectWithError(w, r, nextPage, err)
				return
			}

			// all good, display confirmation
			msg := strings.Title(r.FormValue("minMax")) + " HTLC size updated to " + formatSigned(size) + " sats"
			http.Redirect(w, r, nextPage+"msg="+msg, http.StatusSeeOther)
			return

		case "enableHTTPS":
			if err := config.GenerateServerCertificate(); err == nil {
				// opt-in for a single password auth
				password := ""
				if r.FormValue("enablePassword") == "on" {
					password = r.FormValue("password")
				}
				// restart with HTTPS listener
				restart(w, r, true, password)
			} else {
				redirectWithError(w, r, "/ca?", err)
			}
			return

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

			// Reload liquid page with pop-up
			http.Redirect(w, r, "/liquid?msg="+msg, http.StatusSeeOther)
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
			log.Println("unknown action: ", action)
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

		if os.Getenv("NO_HTTPS") != "true" {
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
					if err := config.GenerateServerCertificate(); err == nil {
						restart(w, r, true, config.Config.Password)
					} else {
						log.Println("GenereateServerCertificate:", err)
						redirectWithError(w, r, "/config?", err)
						return
					}
				}
			}

			if !secureConnection && config.Config.SecureConnection {
				// restart to listen on HTTP only
				restart(w, r, false, "")
			}
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
		Authenticated  bool
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
		Authenticated:  config.Config.SecureConnection && config.Config.Password != "",
		ColorScheme:    config.Config.ColorScheme,
		ErrorMessage:   "",
		PopUpMessage:   "",
		MempoolFeeRate: mempoolFeeRate,
		LogPosition:    0, // new content and wait for connection
		LogFile:        logFile,
		SearchText:     searchText,
	}

	// executing template named "loading"
	executeTemplate(w, "loading", data)
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

func downloadCaHandler(w http.ResponseWriter, r *http.Request) {
	fileName := "CA.crt"
	// Set the Content-Disposition header to suggest a filename
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fileName))
	// Serve the file for download
	http.ServeFile(w, r, filepath.Join(config.Config.DataDir, fileName))
}

// shows peerswapd log
func logHandler(w http.ResponseWriter, r *http.Request) {
	type Page struct {
		Authenticated  bool
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
		Authenticated:  config.Config.SecureConnection && config.Config.Password != "",
		ColorScheme:    config.Config.ColorScheme,
		ErrorMessage:   "",
		PopUpMessage:   "",
		MempoolFeeRate: mempoolFeeRate,
		LogPosition:    1, // from first line
		LogFile:        logFile,
		Implementation: ln.Implementation,
	}

	// executing template named "logpage"
	executeTemplate(w, "logpage", data)
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

func executeTemplate(w io.Writer, name string, data any) {
	err := templates.ExecuteTemplate(w, name, data)
	if err != nil {
		if strings.Contains(err.Error(), "broken pipe") || strings.Contains(err.Error(), "http2: stream closed") {
			// nothing can be done
			return
		} else {
			log.Fatalf("Template execution error: %v", err)
		}
	}
}
