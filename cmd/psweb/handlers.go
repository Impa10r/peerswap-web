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
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"peerswap-web/cmd/psweb/bitcoin"
	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/db"
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
		ClaimJoinInvite   bool
		AdvertiseLiquid   bool
		AdvertiseBitcoin  bool
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
		PeginPending:      config.Config.PeginTxId != "" && config.Config.PeginClaimScript != "",
		ClaimJoinInvite:   ln.ClaimJoinHandler != "",
		AdvertiseLiquid:   ln.AdvertiseLiquidBalance,
		AdvertiseBitcoin:  ln.AdvertiseBitcoinBalance,
	}

	// executing template named "homepage" with retries
	executeTemplate(w, "homepage", data)
}

type Premium struct {
	Asset          int32
	Operation      int32
	PremiumRatePpm int64
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

	senderInProfit := int64(0)
	senderOutProfit := int64(0)
	receiverInProfit := int64(0)
	receiverOutProfit := int64(0)
	cost := int64(0)
	new := false
	persist := false // have new tx fees to persist

	for _, swap := range swaps {
		switch swap.Type + swap.Role {
		case "swap-insender":
			if swap.PeerNodeId == id {
				cost, _, new = swapCost(swap)
				senderInProfit -= cost
			}
		case "swap-outsender":
			if swap.PeerNodeId == id {
				cost, _, new = swapCost(swap)
				senderOutProfit -= cost
			}
		case "swap-outreceiver":
			if swap.InitiatorNodeId == id {
				cost, _, new = swapCost(swap)
				receiverOutProfit -= cost
			}
		case "swap-inreceiver":
			if swap.InitiatorNodeId == id {
				cost, _, new = swapCost(swap)
				receiverInProfit -= cost
			}
		}

		persist = persist || new
	}

	// save to db
	if persist {
		db.Save("Swaps", "txFee", txFee)
	}

	senderInProfitPPM := int64(0)
	receiverInProfitPPM := int64(0)
	receiverOutProfitPPM := int64(0)
	senderOutProfitPPM := int64(0)

	// Get Lightning client
	cl, clean, er := ln.GetClient()
	if er != nil {
		redirectWithError(w, r, "/config?", er)
		return
	}
	defer clean()

	btcBalance := ln.ConfirmedWalletBalance(cl)

	var globalPremium, peerPremium []Premium

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
			senderOutProfitPPM = senderOutProfit * 1_000_000 / int64(peer.AsSender.SatsOut)
		}
		if peer.AsSender.SatsIn > 0 {
			senderInProfitPPM = senderInProfit * 1_000_000 / int64(peer.AsSender.SatsIn)
		}
		if peer.AsReceiver.SatsOut > 0 {
			receiverOutProfitPPM = receiverOutProfit * 1_000_000 / int64(peer.AsReceiver.SatsOut)
		}
		if peer.AsReceiver.SatsIn > 0 {
			receiverInProfitPPM = receiverInProfit * 1_000_000 / int64(peer.AsReceiver.SatsIn)
		}

		for _, asset := range []peerswaprpc.AssetType{peerswaprpc.AssetType_BTC, peerswaprpc.AssetType_LBTC} {
			for _, operation := range []peerswaprpc.OperationType{peerswaprpc.OperationType_SWAP_IN, peerswaprpc.OperationType_SWAP_OUT} {
				globalRate, _ := ps.GetGlobalPremiumRate(client, asset, operation)
				globalPremium = append(globalPremium, Premium{
					Asset:          int32(asset.Number()),
					Operation:      int32(operation.Number()),
					PremiumRatePpm: globalRate.PremiumRatePpm,
				})

				peerRate, _ := ps.GetPremiumRate(client, peer.NodeId, asset, operation)
				peerPremium = append(peerPremium, Premium{
					Asset:          int32(asset.Number()),
					Operation:      int32(operation.Number()),
					PremiumRatePpm: peerRate.PremiumRatePpm,
				})
			}
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
	liquid.ListUnspent(&utxosLBTC, elementsBitcoinId)

	// to find a channel for swap-out
	maxLocalBalance := uint64(0)
	maxLocalBalanceIndex := 0

	// to find a channel for swap-in
	maxRemoteBalance := uint64(0)
	maxRemoteBalanceIndex := 0
	isOnline := false

	// get routing stats
	for i, ch := range peer.Channels {
		stat := ln.GetForwardingStats(ch.ChannelId)
		stats = append(stats, stat)

		info := ln.GetChannelInfo(cl, ch.ChannelId, peer.NodeId)
		info.LocalBalance = ch.GetLocalBalance()
		if info.LocalBalance > maxLocalBalance {
			maxLocalBalance = info.LocalBalance
			maxLocalBalanceIndex = i
		}

		info.RemoteBalance = ch.GetRemoteBalance()
		if info.RemoteBalance > maxRemoteBalance {
			maxRemoteBalance = info.RemoteBalance
			maxRemoteBalanceIndex = i
		}

		info.Active = ch.GetActive()
		isOnline = isOnline || info.Active

		info.LocalPct = info.LocalBalance * 100 / info.Capacity
		channelInfo = append(channelInfo, info)

		sumLocal += ch.GetLocalBalance()
		sumRemote += ch.GetRemoteBalance()

		// should not be less than our Min HTLC setting
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

	feeRate := liquid.EstimateFee()
	if !psPeer {
		feeRate = mempoolFeeRate
	}

	// this is what peerswap will use
	bitcoinFeeRate := ln.EstimateFee()

	// should match peerswap estimations
	swapFeeReserveBTC := int64(math.Ceil(bitcoinFeeRate * OPENING_TX_SIZE_BTC))
	swapFeeReserveLBTC := int64(math.Ceil(feeRate * OPENING_TX_SIZE_BTC))
	if hasDiscountedvSize {
		swapFeeReserveLBTC = int64(math.Ceil(feeRate * OPENING_TX_SIZE_LBTC_DISCOUNTED))
	}

	selectedChannel := peer.Channels[maxRemoteBalanceIndex].ChannelId

	spendable, receivable, err := ln.FetchChannelLimits(cl)

	if err != nil {
		log.Printf("error fetching channel reserves: %v", err)
		redirectWithError(w, r, "/config?", err)
		return
	}

	// haircut to avoid 'no matching outgoing channel available'
	maxLiquidSwapIn := min(int64(satAmount)-int64(SwapLbtcDustReserve), int64(receivable[selectedChannel]))
	if maxLiquidSwapIn < 100_000 {
		maxLiquidSwapIn = 0
	}

	peerLiquidBalance := ""
	maxLiquidSwapOut := uint64(0)
	channelCapacity := peer.Channels[maxRemoteBalanceIndex].RemoteBalance + peer.Channels[maxRemoteBalanceIndex].LocalBalance

	if ptr := ln.LiquidBalances[peer.NodeId]; ptr != nil {
		if ptr.Amount < 100_000 {
			peerLiquidBalance = "<100k"
		} else {
			peerLiquidBalance = "≥" + formatWithThousandSeparators(ptr.Amount)
		}
		maxLiquidSwapOut = uint64(max(0, min(int64(spendable[selectedChannel]), int64(ptr.Amount))-swapFeeReserveLBTC))
	} else {
		maxLiquidSwapOut = uint64(max(0, int64(spendable[selectedChannel])-swapFeeReserveLBTC))
	}

	if maxLiquidSwapOut >= 100_000 {
		selectedChannel = peer.Channels[maxLocalBalanceIndex].ChannelId
		channelCapacity = peer.Channels[maxLocalBalanceIndex].RemoteBalance + peer.Channels[maxLocalBalanceIndex].LocalBalance
	} else {
		maxLiquidSwapOut = 0
	}

	peerBitcoinBalance := ""
	maxBitcoinSwapOut := uint64(0)
	if ptr := ln.BitcoinBalances[peer.NodeId]; ptr != nil {
		if ptr.Amount < 100_000 {
			peerBitcoinBalance = "<100k"
		} else {
			peerBitcoinBalance = "≥" + formatWithThousandSeparators(ptr.Amount)
		}
		maxBitcoinSwapOut = uint64(max(0, min(int64(spendable[selectedChannel]), int64(ptr.Amount))-swapFeeReserveBTC))
	} else {
		maxBitcoinSwapOut = uint64(max(0, int64(spendable[selectedChannel])-swapFeeReserveBTC))
	}

	if maxBitcoinSwapOut >= 100_000 {
		selectedChannel = peer.Channels[maxLocalBalanceIndex].ChannelId
		channelCapacity = peer.Channels[maxLocalBalanceIndex].RemoteBalance + peer.Channels[maxLocalBalanceIndex].LocalBalance
	} else {
		maxBitcoinSwapOut = 0
	}

	// haircuts to avoid 'no matching outgoing channel available'
	maxBitcoinSwapIn := min(btcBalance-swapFeeReserveBTC, int64(receivable[selectedChannel]))
	if maxBitcoinSwapIn < 100_000 {
		maxBitcoinSwapIn = 0
	}

	// assumed direction of the swap
	directionIn := true

	// assume return to 50/50 channel
	recommendLiquidSwapOut := uint64(0)
	recommendBitcoinSwapOut := uint64(0)
	if maxLocalBalance > channelCapacity/2 {
		recommendLiquidSwapOut = min(maxLiquidSwapOut, maxLocalBalance-channelCapacity/2)
		recommendBitcoinSwapOut = min(maxBitcoinSwapOut, maxLocalBalance-channelCapacity/2)
	}

	if recommendLiquidSwapOut < 100_000 {
		if maxLiquidSwapOut >= 100_000 {
			recommendLiquidSwapOut = 100_000
		} else {
			recommendLiquidSwapOut = 0
		}
	}

	if recommendBitcoinSwapOut < 100_000 {
		if maxBitcoinSwapOut >= 100_000 {
			recommendBitcoinSwapOut = 100_000
		} else {
			recommendBitcoinSwapOut = 0
		}
	}

	// assume return to 50/50 channel
	recommendLiquidSwapIn := int64(0)
	recommendBitcoinSwapIn := int64(0)
	if maxRemoteBalance > channelCapacity/2 {
		recommendLiquidSwapIn = min(maxLiquidSwapIn, int64(maxRemoteBalance-channelCapacity/2))
		recommendBitcoinSwapIn = min(maxBitcoinSwapIn, int64(maxRemoteBalance-channelCapacity/2))
	} else {
		if recommendLiquidSwapOut > 0 {
			directionIn = false
		}
	}

	if recommendLiquidSwapIn < 100_000 {
		if maxLiquidSwapIn >= 100_000 {
			recommendLiquidSwapIn = 100_000
		} else {
			recommendLiquidSwapIn = 0
		}
	}

	if recommendBitcoinSwapIn < 100_000 {
		if maxBitcoinSwapIn >= 100_000 {
			recommendBitcoinSwapIn = 100_000
		} else {
			recommendBitcoinSwapIn = 0
		}
	}

	type Page struct {
		Authenticated                   bool
		ErrorMessage                    string
		PopUpMessage                    string
		MempoolFeeRate                  float64
		BtcFeeRate                      float64
		ColorScheme                     string
		Peer                            *peerswaprpc.PeerSwapPeer
		PeerAlias                       string
		NodeUrl                         string
		Allowed                         bool
		Suspicious                      bool
		LBTC                            bool
		BTC                             bool
		LiquidBalance                   uint64
		BitcoinBalance                  uint64
		ActiveSwaps                     string
		DirectionIn                     bool
		Stats                           []*ln.ForwardingStats
		ChannelInfo                     []*ln.ChanneInfo
		PeerSwapPeer                    bool
		MyAlias                         string
		SenderOutProfit                 int64
		SenderOutProfitPPM              int64
		SenderInProfit                  int64
		ReceiverInProfit                int64
		ReceiverOutProfit               int64
		SenderInProfitPPM               int64
		ReceiverInProfitPPM             int64
		ReceiverOutProfitPPM            int64
		KeysendSats                     uint64
		OutputsBTC                      *[]ln.UTXO
		OutputsLBTC                     *[]liquid.UTXO
		HasInboundFees                  bool
		PeerBitcoinBalance              string // "" means no data
		MaxBitcoinSwapOut               uint64
		RecommendBitcoinSwapOut         uint64
		MaxBitcoinSwapIn                int64
		RecommendBitcoinSwapIn          int64
		PeerLiquidBalance               string // "" means no data
		MaxLiquidSwapOut                uint64
		RecommendLiquidSwapOut          uint64
		MaxLiquidSwapIn                 int64
		RecommendLiquidSwapIn           int64
		SelectedChannel                 uint64
		HasDiscountedvSize              bool
		RedColor                        string
		IsOnline                        bool
		AnchorReserve                   uint64
		LiquidReserve                   uint64
		OPENING_TX_SIZE_BTC             int64
		OPENING_TX_SIZE_LBTC            int64
		OPENING_TX_SIZE_LBTC_DISCOUNTED int64
		GlobalPremium                   []Premium
		PeerPremium                     []Premium
	}

	redColor := "red"
	if config.Config.ColorScheme == "dark" {
		redColor = "pink"
	}

	data := Page{
		Authenticated:                   config.Config.SecureConnection && config.Config.Password != "",
		ErrorMessage:                    errorMessage,
		PopUpMessage:                    popupMessage,
		BtcFeeRate:                      bitcoinFeeRate,
		MempoolFeeRate:                  feeRate,
		ColorScheme:                     config.Config.ColorScheme,
		Peer:                            peer,
		PeerAlias:                       getNodeAlias(peer.NodeId),
		NodeUrl:                         config.Config.NodeApi,
		Allowed:                         stringIsInSlice(peer.NodeId, allowlistedPeers),
		Suspicious:                      stringIsInSlice(peer.NodeId, suspiciousPeers),
		BTC:                             stringIsInSlice("btc", peer.SupportedAssets),
		LBTC:                            stringIsInSlice("lbtc", peer.SupportedAssets),
		LiquidBalance:                   satAmount,
		BitcoinBalance:                  uint64(btcBalance),
		ActiveSwaps:                     convertSwapsToHTMLTable(activeSwaps, "", "", ""),
		DirectionIn:                     directionIn,
		Stats:                           stats,
		ChannelInfo:                     channelInfo,
		PeerSwapPeer:                    psPeer,
		MyAlias:                         ln.MyNodeAlias,
		SenderOutProfit:                 senderOutProfit,
		SenderOutProfitPPM:              senderOutProfitPPM,
		SenderInProfit:                  senderInProfit,
		ReceiverInProfit:                receiverInProfit,
		ReceiverOutProfit:               receiverOutProfit,
		SenderInProfitPPM:               senderInProfitPPM,
		ReceiverInProfitPPM:             receiverInProfitPPM,
		ReceiverOutProfitPPM:            receiverOutProfitPPM,
		KeysendSats:                     keysendSats,
		OutputsBTC:                      &utxosBTC,
		OutputsLBTC:                     &utxosLBTC,
		HasInboundFees:                  ln.HasInboundFees(),
		PeerBitcoinBalance:              peerBitcoinBalance,
		MaxBitcoinSwapOut:               maxBitcoinSwapOut,
		RecommendBitcoinSwapOut:         recommendBitcoinSwapOut,
		MaxBitcoinSwapIn:                maxBitcoinSwapIn,
		RecommendBitcoinSwapIn:          recommendBitcoinSwapIn,
		PeerLiquidBalance:               peerLiquidBalance,
		MaxLiquidSwapOut:                maxLiquidSwapOut,
		RecommendLiquidSwapOut:          recommendLiquidSwapOut,
		MaxLiquidSwapIn:                 maxLiquidSwapIn,
		RecommendLiquidSwapIn:           recommendLiquidSwapIn,
		SelectedChannel:                 selectedChannel,
		HasDiscountedvSize:              hasDiscountedvSize,
		RedColor:                        redColor,
		IsOnline:                        isOnline,
		AnchorReserve:                   ANCHOR_RESERVE,
		LiquidReserve:                   uint64(SwapLbtcDustReserve),
		OPENING_TX_SIZE_BTC:             OPENING_TX_SIZE_BTC,
		OPENING_TX_SIZE_LBTC:            OPENING_TX_SIZE_LBTC,
		OPENING_TX_SIZE_LBTC_DISCOUNTED: OPENING_TX_SIZE_LBTC_DISCOUNTED,
		GlobalPremium:                   globalPremium,
		PeerPremium:                     peerPremium,
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

	// newly genetated address
	addr := ""
	keys, ok = r.URL.Query()["addr"]
	if ok && len(keys[0]) > 0 {
		addr = keys[0]
	}

	cl, clean, er := ln.GetClient()
	if er != nil {
		redirectWithError(w, r, "/config?", er)
		return
	}
	defer clean()

	type Page struct {
		Authenticated       bool
		ErrorMessage        string
		PopUpMessage        string
		ColorScheme         string
		BitcoinBalance      uint64
		Outputs             *[]ln.UTXO
		PeginTxId           string
		IsPegin             bool // false for ordinary BTC withdrawal
		IsExternal          bool
		PeginAddress        string
		PeginAmount         uint64
		BitcoinApi          string
		Confirmations       int32
		TargetConfirmations int32
		Progress            int32
		ETA                 string
		FeeRate             float64
		LiquidFeeRate       float64
		MempoolFeeRate      float64
		SuggestedFeeRate    float64
		MinBumpFeeRate      float64
		CanBump             bool
		CanRBF              bool
		IsCLN               bool
		BitcoinAddress      string
		AdvertiseEnabled    bool
		BitcoinSwaps        bool
		HasDiscountedvSize  bool
		CanClaimJoin        bool
		IsClaimJoin         bool
		ClaimJoinStatus     string
		HasClaimJoinPending bool
		ClaimJoinHours      int
		ClaimJointTimeLimit string
	}

	btcBalance := ln.ConfirmedWalletBalance(cl)
	fee := float64(mempoolFeeRate)
	confs := int32(0)
	canBump := false
	canCPFP := false

	var utxos []ln.UTXO
	ln.ListUnspent(cl, &utxos, int32(1))

	isExternal := config.Config.PeginTxId == "external"

	if config.Config.PeginTxId != "" {
		if !isExternal {
			confs, canCPFP = peginConfirmations(config.Config.PeginTxId)
			// update ClaimJoin status
			checkPegin()
			if confs == 0 && config.Config.PeginFeeRate > 0 {
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
	}

	duration := time.Duration(10*(int32(peginBlocks)-confs)) * time.Minute
	maxConfs := int32(peginBlocks)
	cjHours := 34
	cjTimeLimit := ""

	currentBlockHeight := int32(ln.GetBlockHeight())
	if ln.MyRole != "none" {
		target := int32(ln.ClaimBlockHeight)
		maxConfs = target - currentBlockHeight + confs
		duration = time.Duration(10*(target-currentBlockHeight)) * time.Minute
	} else if ln.ClaimJoinHandler != "" {
		cjHours = int((int32(ln.JoinBlockHeight) - currentBlockHeight + int32(peginBlocks)) / 6)
		cjTimeLimit = time.Now().Add(time.Duration(10*(ln.JoinBlockHeight-uint32(currentBlockHeight))) * time.Minute).Format("3:04 PM")
	}

	progress := confs * 100 / int32(maxConfs)

	eta := time.Now().Add(duration).Format("3:04 PM")
	if duration < 0 {
		eta = "Past due"
	}

	data := Page{
		Authenticated:       config.Config.SecureConnection && config.Config.Password != "",
		ErrorMessage:        errorMessage,
		PopUpMessage:        popupMessage,
		ColorScheme:         config.Config.ColorScheme,
		BitcoinBalance:      uint64(btcBalance),
		Outputs:             &utxos,
		PeginTxId:           config.Config.PeginTxId,
		IsPegin:             config.Config.PeginClaimScript != "",
		IsExternal:          isExternal,
		PeginAddress:        config.Config.PeginAddress,
		PeginAmount:         uint64(config.Config.PeginAmount),
		BitcoinApi:          config.Config.BitcoinApi,
		Confirmations:       confs,
		TargetConfirmations: maxConfs,
		Progress:            progress,
		ETA:                 eta,
		FeeRate:             config.Config.PeginFeeRate,
		MempoolFeeRate:      mempoolFeeRate,
		LiquidFeeRate:       liquid.EstimateFee(),
		SuggestedFeeRate:    math.Ceil(fee*100) / 100,
		MinBumpFeeRate:      math.Ceil((config.Config.PeginFeeRate+1)*100) / 100,
		CanBump:             canBump,
		CanRBF:              ln.CanRBF(),
		IsCLN:               ln.IMPLEMENTATION == "CLN",
		BitcoinAddress:      addr,
		AdvertiseEnabled:    ln.AdvertiseBitcoinBalance,
		BitcoinSwaps:        config.Config.BitcoinSwaps,
		CanClaimJoin:        hasDiscountedvSize,
		IsClaimJoin:         config.Config.PeginClaimJoin,
		ClaimJoinStatus:     ln.ClaimStatus,
		HasClaimJoinPending: ln.ClaimJoinHandler != "",
		ClaimJointTimeLimit: cjTimeLimit,
		ClaimJoinHours:      cjHours,
	}

	// executing template named "bitcoin"
	executeTemplate(w, "bitcoin", data)
}

// returns number of confirmations and whether the tx can be fee bumped
func peginConfirmations(txid string) (int32, bool) {

	// can be external funding
	var tx bitcoin.Transaction
	_, err := bitcoin.GetRawTransaction(txid, &tx)
	if err == nil {
		return tx.Confirmations, len(tx.Vout) > 1
	}

	// -1 indicates error
	return -1, false
}

// handles Liquid peg-in and Bitcoin send form
func peginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// Parse the form data
		err := r.ParseForm()
		if err != nil {
			http.Error(w, "Error parsing form data", http.StatusBadRequest)
			return
		}

		isPegin := r.FormValue("isPegin") == "true"
		isExternal := r.FormValue("externalButton") != ""

		var (
			amount int64
			fee    float64
		)

		selectedOutputs := r.Form["selected_outputs[]"]
		subtractFeeFromAmount := r.FormValue("subtractfee") == "on"

		if !isExternal {
			if r.FormValue("peginAmount") == "" {
				redirectWithError(w, r, "/bitcoin?", errors.New("amount cannot be blank"))
				return
			}

			amount, err = strconv.ParseInt(r.FormValue("peginAmount"), 10, 64)
			if err != nil {
				redirectWithError(w, r, "/bitcoin?", err)
				return
			}

			if r.FormValue("feeRate") == "" {
				redirectWithError(w, r, "/bitcoin?", errors.New("fee rate cannot be blank"))
				return
			}

			fee, err = strconv.ParseFloat(r.FormValue("feeRate"), 64)
			if err != nil {
				redirectWithError(w, r, "/bitcoin?", err)
				return
			}

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
		}

		address := ""
		claimScript := ""
		config.Config.PeginClaimJoin = false

		if isPegin {
			// check that elements is fully synced
			info, err := liquid.GetBlockchainInfo()
			if err != nil {
				redirectWithError(w, r, "/bitcoin?", err)
				return
			}
			if info.InitialBlockDownload {
				redirectWithError(w, r, "/bitcoin?", errors.New("elements initial block download is not complete"))
				return
			}

			// test on a pre-existing tx that bitcon core can complete the peg
			tx := "b61ec844027ce18fd3eb91fa7bed8abaa6809c4d3f6cf4952b8ebaa7cd46583a"
			if config.Config.Chain == "testnet" {
				// identify testnet blockchain
				genesisHash, err := bitcoin.GetBlockHash(0)
				if err != nil {
					redirectWithError(w, r, "/bitcoin?", err)
					return
				}

				if genesisHash == "000000000933ea01ad0ee984209779baaec3ced90fa3f408719526f8d77f4943" {
					// testnet3
					tx = "2c7ec5043fe8ee3cb4ce623212c0e52087d3151c9e882a04073cce1688d6fc1e"
				} else {
					// testnet4
					tx = "0b387c3b7a8d9fad4d7a1ac2cba4958451c03d6c4fe63dfbe10cdb86d666cdd7"
				}
			}

			_, err = bitcoin.GetTxOutProof(tx)
			if err != nil {
				// try again
				time.Sleep(2 * time.Second)
				_, err = bitcoin.GetTxOutProof(tx)
			}

			if err != nil && config.Config.Chain != "signet" {
				if !strings.HasPrefix(config.Config.BitcoinHost, "https://go.getblock.io/") {
					// save old settings
					host := config.Config.BitcoinHost
					user := config.Config.BitcoinUser
					pass := config.Config.BitcoinPass
					// automatic fallback to getblock.io
					config.Config.BitcoinHost = config.GetBlockIoHost()
					config.Config.BitcoinUser = ""
					config.Config.BitcoinPass = ""
					_, err2 := bitcoin.GetTxOutProof(tx)
					if err2 != nil {
						// revert settings
						config.Config.BitcoinHost = host
						config.Config.BitcoinUser = user
						config.Config.BitcoinPass = pass
						redirectWithError(w, r, "/config?", errors.New("GetTxOutProof failed: "+err.Error()))
						return
					} else {
						// use getblock.io endpoint going forward
						log.Println("Switching to getblock.io bitcoin host endpoint")
						if err := config.Save(); err != nil {
							redirectWithError(w, r, "/bitcoin?", err)
							return
						}
					}
				} else {
					redirectWithError(w, r, "/bitcoin?", errors.New("GetTxOutProof failed: "+err.Error()))
					return
				}
			}

			addr, err := liquid.GetPeginAddress()
			if err != nil {
				redirectWithError(w, r, "/bitcoin?", err)
				return
			}

			address = addr.MainChainAddress
			claimScript = addr.ClaimScript

			if hasDiscountedvSize {
				config.Config.PeginClaimJoin = r.FormValue("claimJoin") == "on"
				if config.Config.PeginClaimJoin {
					ln.ClaimStatus = "Awaiting funding tx to confirm"
					db.Save("ClaimJoin", "ClaimStatus", ln.ClaimStatus)
				}
			}
		} else {
			address = r.FormValue("sendAddress")
			claimScript = ""
		}

		if !isExternal {
			label := "Liquid Pegin"
			if !isPegin {
				label = "BTC Withdrawal"
			}

			res, err := ln.SendCoinsWithUtxos(&selectedOutputs, address, amount, fee, subtractFeeFromAmount, label)
			if err != nil {
				redirectWithError(w, r, "/bitcoin?", err)
				return
			}

			if isPegin {
				log.Println("New Peg-in TxId:", res.TxId, "RawHex:", res.RawHex, "Claim script:", claimScript)
				duration := time.Duration(10*peginBlocks) * time.Minute
				eta := time.Now().Add(duration).Format("3:04 PM")
				telegramSendMessage(fmt.Sprintf("⏰ Started peg in %s sats, fee rate: %0.2f s/vb, ETA: %s, TxId: `%s`", formatWithThousandSeparators(uint64(res.AmountSat)), config.Config.PeginFeeRate, eta, res.TxId))
			} else {
				log.Println("BTC withdrawal pending, TxId:", res.TxId, "RawHex:", res.RawHex)
				telegramSendMessage(fmt.Sprintf("⛓️ BTC withdrawal pending: %s sats, fee rate: %0.2f s/vb, TxId: `%s`", formatWithThousandSeparators(uint64(res.AmountSat)), config.Config.PeginFeeRate, res.TxId))
			}
			config.Config.PeginAmount = res.AmountSat
			config.Config.PeginTxId = res.TxId
			config.Config.PeginFeeRate = res.ExactSatVb
		} else {
			log.Println("Peg-in address for external funding:", address, "Claim script:", claimScript)
			config.Config.PeginTxId = "external"
		}

		config.Config.PeginClaimScript = claimScript
		config.Config.PeginAddress = address
		config.Config.PeginReplacedTxId = ""

		if err := config.Save(); err != nil {
			redirectWithError(w, r, "/bitcoin?", err)
			return
		}

		// Redirect to bitcoin page to follow the peg-in progress
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

		fee, err := strconv.ParseFloat(r.FormValue("feeRate"), 64)
		if err != nil {
			redirectWithError(w, r, "/bitcoin?", err)
			return
		}

		if config.Config.PeginTxId == "" || config.Config.PeginTxId == "external" {
			redirectWithError(w, r, "/bitcoin?", errors.New("no pending peg-in"))
			return
		}

		confs, _ := peginConfirmations(config.Config.PeginTxId)
		if confs > 0 {
			// transaction has been confirmed already
			http.Redirect(w, r, "/bitcoin", http.StatusSeeOther)
			return
		}

		label := "Liquid Peg-in"
		if config.Config.PeginClaimScript == "" {
			label = "BTC Withdrawal"
		}

		res, err := ln.BumpPeginFee(fee, label)
		if err != nil {
			redirectWithError(w, r, "/bitcoin?", err)
			return
		}

		if ln.CanRBF() {
			log.Println("RBF TxId:", res.TxId, "RawHex:", res.RawHex)
			config.Config.PeginReplacedTxId = config.Config.PeginTxId
			config.Config.PeginAmount = res.AmountSat
			config.Config.PeginTxId = res.TxId
		} else {
			// txid not available, let's hope LND broadcasted it fine
			log.Println("CPFP initiated")
		}

		// save the new rate, so the next bump cannot be lower
		config.Config.PeginFeeRate = res.ExactSatVb

		if err := config.Save(); err != nil {
			redirectWithError(w, r, "/bitcoin?", err)
			return
		}

		// Redirect to bitcoin page to follow the peg-in progress
		http.Redirect(w, r, "/bitcoin?msg=New transaction broadcasted", http.StatusSeeOther)
	} else {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

type FeeLog struct {
	Alias     string
	ChannelId uint64
	TimeStamp int64
	TimeUTC   string
	TimeAgo   string
	OldRate   int64
	NewRate   int64
	IsInbound bool
	IsManual  bool
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
	currentTime := time.Now()
	persistNodeIds := false

	for _, peer := range res.GetPeers() {
		alias := getNodeAlias(peer.NodeId)
		for _, ch := range peer.Channels {
			rule, custom := ln.AutoFeeRatesSummary(ch.ChannelId)
			af, _ := ln.AutoFeeRule(ch.ChannelId)

			if peerNodeId[ch.ChannelId] == "" {
				peerNodeId[ch.ChannelId] = peer.NodeId
				persistNodeIds = true
			}

			daysNoFlow := 999
			ts, ok := ln.LastForwardTS.Read(ch.ChannelId)
			if ok {
				daysNoFlow = int(currentTime.Sub(time.Unix(ts, 0)).Hours() / 24)
			}

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
				DaysNoFlow:  daysNoFlow,
				Active:      ch.Active,
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

	// persist Node Ids to db for offline and closed channels retrieval
	if persistNodeIds {
		db.Save("Peers", "NodeId", peerNodeId)
	}

	if peerId == "" {
		// non-existing channel
		channelId = 0
	}

	// sort by LocalPct ascending
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
		(*chart)[i].Label = "Routed: " + formatWithThousandSeparators(p.Amount) + ", Fee: " + formatFloat(p.Fee) + ", PPM: " + formatWithThousandSeparators(p.PPM)
	}

	var feeLog []FeeLog

	// 24 hours fee log for all channels
	days := 1
	if channelId > 0 {
		// or 30 days for a single one
		days = 30
	}
	startTS := time.Now().AddDate(0, 0, -days).Unix()

	for id := range ln.AutoFeeLog {
		for _, event := range ln.AutoFeeLog[id] {
			if event.TimeStamp > startTS {
				// either all or specific channel
				if channelId == 0 || channelId == id {
					timeAgo := timePassedAgo(time.Unix(event.TimeStamp, 0))
					feeLog = append(feeLog, FeeLog{
						TimeStamp: event.TimeStamp,
						TimeUTC:   time.Unix(event.TimeStamp, 0).UTC().Format(time.RFC1123),
						TimeAgo:   timeAgo,
						Alias:     getNodeAlias(peerNodeId[id]),
						ChannelId: id,
						OldRate:   int64(event.OldRate),
						NewRate:   int64(event.NewRate),
						IsInbound: event.IsInbound,
						IsManual:  event.IsManual,
					})
				}
			}
		}
	}

	// sort by TimeStamp descending
	sort.Slice(feeLog, func(i, j int) bool {
		return feeLog[i].TimeStamp > feeLog[j].TimeStamp
	})

	forwardsLog := ln.ForwardsLog(channelId, startTS)

	for i, f := range *forwardsLog {
		(*forwardsLog)[i].AliasIn = getNodeAlias(peerNodeId[f.ChanIdIn])
		(*forwardsLog)[i].AliasOut = getNodeAlias(peerNodeId[f.ChanIdOut])
		(*forwardsLog)[i].Inbound = (*forwardsLog)[i].AliasIn == peerName
		(*forwardsLog)[i].Outbound = (*forwardsLog)[i].AliasOut == peerName
		(*forwardsLog)[i].TimeAgo = timePassedAgo(time.Unix(int64(f.TS), 0))
		(*forwardsLog)[i].TimeUTC = time.Unix(int64(f.TS), 0).UTC().Format(time.RFC1123)
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
		FeeLog         []FeeLog
		ForwardsLog    *[]ln.DataPoint
		RedColor       string
		GreenColor     string
	}

	redColor := "red"
	greenColor := "green"
	if config.Config.ColorScheme == "dark" {
		redColor = "pink"
		greenColor = "lightgreen"
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
		FeeLog:         feeLog,
		ForwardsLog:    forwardsLog,
		RedColor:       redColor,
		GreenColor:     greenColor,
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

	// refresh swap rebate
	ln.GetChannelStats(swap.LndChanId, uint64(time.Now().Add(-time.Hour).Unix()))

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

	cost, breakdown, persist := swapCost(swap)
	if cost != 0 {
		ppm := cost * 1_000_000 / int64(swap.Amount)

		swapData += `<tr><td style="text-align: right">Swap `
		if cost >= 0 {
			swapData += `Cost`
		} else {
			swapData += `Profit`
			cost = -cost
			ppm = -ppm
		}

		swapData += `:</td><td>`
		swapData += formatSigned(cost) + " sats</td></tr>"

		swapData += `<tr><td style="text-align: right">Breakdown:</td>`
		swapData += `<td>` + breakdown + `</td></tr>`

		if swap.State == "State_ClaimedPreimage" {
			swapData += `<tr><td style="text-align: right">PPM:</td><td>`
			swapData += formatSigned(ppm)
		}

		// save to db
		if persist {
			db.Save("Swaps", "txFee", txFee)
		}
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
		Version:         VERSION,
		Latest:          latestVersion,
		Implementation:  ln.IMPLEMENTATION,
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

	var candidate AutoSwapParams

	if err := findSwapInCandidate(&candidate); err != nil {
		log.Printf("unable findSwapInCandidate: %v", err)
		redirectWithError(w, r, "/liquid?", err)
		return
	}

	walletInfo, err := liquid.GetWalletInfo()
	if err != nil {
		redirectWithError(w, r, "/?", err)
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
		AutoSwapMaxAmount       uint64
		AutoSwapThresholdPPM    uint64
		AutoSwapCandidate       *AutoSwapParams
		AutoSwapTargetPct       uint64
		AutoSwapPremiumLimit    int64
		AdvertiseEnabled        bool
		DescriptorsWallet       bool
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
		AutoSwapMaxAmount:       config.Config.AutoSwapMaxAmount,
		AutoSwapThresholdPPM:    config.Config.AutoSwapThresholdPPM,
		AutoSwapTargetPct:       config.Config.AutoSwapTargetPct,
		AutoSwapPremiumLimit:    config.Config.AutoSwapPremiumLimit,
		AutoSwapCandidate:       &candidate,
		AdvertiseEnabled:        ln.AdvertiseLiquidBalance,
		DescriptorsWallet:       walletInfo.Descriptors,
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
		case "setPremium":
			nextPage := r.FormValue("nextPage")

			isDeleted := r.FormValue("premium") == ""

			premiumRatePpm, _ := strconv.ParseInt(r.FormValue("premium"), 10, 64)

			asset, err := strconv.ParseInt(r.FormValue("asset"), 10, 32)
			if err != nil {
				redirectWithError(w, r, nextPage, err)
				return
			}

			operation, err := strconv.ParseInt(r.FormValue("operation"), 10, 32)
			if err != nil {
				redirectWithError(w, r, nextPage, err)
				return
			}

			nodeId := r.FormValue("peerNodeId")
			which := "Default"
			what := "deleted"
			var result *peerswaprpc.PremiumRate

			if nodeId == "" {
				result, err = ps.UpdateGlobalPremiumRate(client, &peerswaprpc.PremiumRate{
					Asset:          peerswaprpc.AssetType(asset),
					Operation:      peerswaprpc.OperationType(operation),
					PremiumRatePpm: premiumRatePpm,
				})
				what = fmt.Sprintf("updated to %d", result.GetPremiumRatePpm())
			} else {
				which = "Peer's"
				if isDeleted {
					result, err = ps.DeletePremiumRate(client, nodeId,
						&peerswaprpc.PremiumRate{
							Asset:     peerswaprpc.AssetType(asset),
							Operation: peerswaprpc.OperationType(operation),
						})
				} else {
					result, err = ps.UpdatePremiumRate(client, nodeId,
						&peerswaprpc.PremiumRate{
							Asset:          peerswaprpc.AssetType(asset),
							Operation:      peerswaprpc.OperationType(operation),
							PremiumRatePpm: premiumRatePpm,
						})
					what = fmt.Sprintf("updated to %d", result.GetPremiumRatePpm())
				}
			}

			if err != nil || result == nil {
				redirectWithError(w, r, nextPage, err)
				return
			}

			// all good, display confirmation
			msg := fmt.Sprintf("%s %s %s premium rate %s", which, result.Asset.String(), result.Operation.String(), what)
			http.Redirect(w, r, nextPage+"msg="+msg, http.StatusSeeOther)
			return

		case "externalPeginTxId":
			if r.FormValue("externalPeginCancel") != "" {
				config.Config.PeginTxId = ""
				config.Config.PeginClaimJoin = false
			} else {
				txid := r.FormValue("peginTxId")
				if txid == "" {
					redirectWithError(w, r, "/bitcoin?", errors.New("TxId is blank"))
					return
				}

				// find the funding output
				var tx bitcoin.Transaction
				_, err := bitcoin.GetRawTransaction(txid, &tx)
				if err != nil {
					redirectWithError(w, r, "/bitcoin?", err)
					return
				}

				found := false
				for _, out := range tx.Vout {
					if out.ScriptPubKey.Address == config.Config.PeginAddress {
						found = true
						config.Config.PeginAmount = int64(toSats(out.Value))
						break
					}
				}

				if !found {
					redirectWithError(w, r, "/bitcoin?", errors.New("the tx fails to pay the pegin address"))
					return
				}

				config.Config.PeginTxId = txid
				config.Config.PeginFeeRate = 0
				ln.ClaimStatus = "Awaiting funding tx to confirm"

				log.Println("External Funding TxId:", txid)
				duration := time.Duration(10*(int32(peginBlocks)-tx.Confirmations)) * time.Minute
				eta := time.Now().Add(duration).Format("3:04 PM")
				telegramSendMessage("⏰ Started peg in " + formatWithThousandSeparators(uint64(config.Config.PeginAmount)) + " sats. ETA: " + eta + ". TxId: `" + txid + "`")
			}

			config.Save()

			// all done, display tx confirmations
			http.Redirect(w, r, "/bitcoin", http.StatusSeeOther)
			return

		case "advertiseLiquidBalance":
			enabled := r.FormValue("enabled") == "on"
			if enabled && !config.Config.AllowSwapRequests {
				redirectWithError(w, r, "/liquid?", errors.New("liquid swap requests are disabled"))
				return
			}

			ln.AdvertiseLiquidBalance = enabled
			db.Save("Peers", "AdvertiseLiquidBalance", ln.AdvertiseLiquidBalance)

			msg := "Broadcasting Liquid Balance is "

			if ln.AdvertiseLiquidBalance {
				msg += "Enabled"
			} else {
				msg += "Disabled"
			}

			// all done, display confirmation
			http.Redirect(w, r, "/liquid?msg="+msg, http.StatusSeeOther)
			return

		case "advertiseBitcoinBalance":
			enabled := r.FormValue("enabled") == "on"
			if enabled && (!config.Config.AllowSwapRequests || !config.Config.BitcoinSwaps) {
				redirectWithError(w, r, "/bitcoin?", errors.New("bitcoin swap requests are disabled on configuration page"))
				return
			}

			ln.AdvertiseBitcoinBalance = enabled
			db.Save("Peers", "AdvertiseBitcoinBalance", ln.AdvertiseBitcoinBalance)

			msg := "Broadcasting Bitcoin Balance is "

			if ln.AdvertiseBitcoinBalance {
				msg += "Enabled"
			} else {
				msg += "Disabled"
			}

			// all done, display confirmation
			http.Redirect(w, r, "/bitcoin?msg="+msg, http.StatusSeeOther)
			return

		case "deleteTxId":
			// acknowledges BTC withdrawal
			config.Config.PeginTxId = ""
			if err := config.Save(); err != nil {
				redirectWithError(w, r, "/bitcoin?", err)
				return
			}

			// all done
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		case "saveAutoFee":
			channelId, err := strconv.ParseUint(r.FormValue("channelId"), 10, 64)
			if err != nil {
				redirectWithError(w, r, "/af?", err)
				return
			}

			var newRule ln.AutoFeeParams

			newRule.FailedBumpPPM, err = strconv.Atoi(r.FormValue("failBump"))
			if err != nil {
				redirectWithError(w, r, "/af?", err)
				return
			}

			newRule.FailedMoveThreshold, err = strconv.Atoi(r.FormValue("failedMoveThreshold"))
			if err != nil {
				redirectWithError(w, r, "/af?", err)
				return
			}

			newRule.LowLiqPct, err = strconv.Atoi(r.FormValue("lowLiqPct"))
			if err != nil {
				redirectWithError(w, r, "/af?", err)
				return
			}

			newRule.LowLiqRate, err = strconv.Atoi(r.FormValue("lowLiqRate"))
			if err != nil {
				redirectWithError(w, r, "/af?", err)
				return
			}

			newRule.NormalRate, err = strconv.Atoi(r.FormValue("normalRate"))
			if err != nil {
				redirectWithError(w, r, "/af?", err)
				return
			}

			newRule.ExcessPct, err = strconv.Atoi(r.FormValue("excessPct"))
			if err != nil {
				redirectWithError(w, r, "/af?", err)
				return
			}

			newRule.ExcessRate, err = strconv.Atoi(r.FormValue("excessRate"))
			if err != nil {
				redirectWithError(w, r, "/af?", err)
				return
			}

			newRule.InactivityDays, err = strconv.Atoi(r.FormValue("inactivityDays"))
			if err != nil {
				redirectWithError(w, r, "/af?", err)
				return
			}

			newRule.InactivityDropPPM, err = strconv.Atoi(r.FormValue("inactivityDropPPM"))
			if err != nil {
				redirectWithError(w, r, "/af?", err)
				return
			}

			newRule.InactivityDropPct, err = strconv.Atoi(r.FormValue("inactivityDropPct"))
			if err != nil {
				redirectWithError(w, r, "/af?", err)
				return
			}

			newRule.CoolOffHours, err = strconv.Atoi(r.FormValue("coolOffHours"))
			if err != nil {
				redirectWithError(w, r, "/af?", err)
				return
			}

			if ln.HasInboundFees() {
				newRule.LowLiqDiscount, err = strconv.Atoi(r.FormValue("lowLiqDiscount"))
				if err != nil {
					redirectWithError(w, r, "/af?", err)
					return
				}
			}
			rule := &ln.AutoFeeDefaults
			msg := ""
			updateAll := false

			if _, isCustom := ln.AutoFeeRule(channelId); isCustom {
				updateAll = r.FormValue("update_all") != ""
			}

			if updateAll {
				msg = "All custom rules updated:"
				old := reflect.ValueOf(*ln.AutoFee[channelId])
				new := reflect.ValueOf(newRule)

				// find what will be updated
				for i := 0; i < old.NumField(); i++ {
					if old.Field(i).Int() != new.Field(i).Int() {
						msg += fmt.Sprintf(" %s=%v", new.Type().Field(i).Name, new.Field(i).Interface())
					}
				}

				for _, rulePtr := range ln.AutoFee {
					if rulePtr == nil {
						continue
					}
					// Use Elem() to get the underlying struct from the pointer
					current := reflect.ValueOf(rulePtr).Elem()

					for i := 0; i < old.NumField(); i++ {
						if old.Field(i).Int() != new.Field(i).Int() {
							if current.Field(i).CanSet() {
								current.Field(i).SetInt(new.Field(i).Int())
							} else {
								redirectWithError(w, r, "/af?", errors.New("unable to set the value of "+current.Type().Field(i).Name))
								return
							}
						}
					}
				}

				// persist to db
				db.Save("AutoFees", "AutoFee", ln.AutoFee)

			} else if r.FormValue("update_button") != "" {
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

				// clone the new data
				*rule = newRule

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
				if ln.IMPLEMENTATION == "CLN" || !ln.CanRBF() {
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

		case "enableHTTPS":
			if err := config.GenerateServerCertificate(); err == nil {
				// opt-in for a single password auth
				password := ""
				if r.FormValue("enablePassword") == "on" {
					password = r.FormValue("password")
				}
				// restart with HTTPS listener
				showRestartScreen(w, r, true, password, true)
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

			maxAmount, err := strconv.ParseUint(r.FormValue("maxAmount"), 10, 64)
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

			newPremiumLimit, err := strconv.ParseInt(r.FormValue("premiumLimit"), 10, 64)
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
				config.Config.AutoSwapMaxAmount != maxAmount ||
				config.Config.AutoSwapThresholdPPM != newPPM ||
				config.Config.AutoSwapTargetPct != newPct ||
				config.Config.AutoSwapPremiumLimit != newPremiumLimit) {
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
			config.Config.AutoSwapMaxAmount = maxAmount
			config.Config.AutoSwapTargetPct = newPct
			config.Config.AutoSwapEnabled = nowEnabled
			config.Config.AutoSwapPremiumLimit = newPremiumLimit

			// Save config
			if err := config.Save(); err != nil {
				redirectWithError(w, r, "/liquid?", err)
				return
			}

			// Reload liquid page with pop-up
			http.Redirect(w, r, "/liquid?msg="+msg, http.StatusSeeOther)
			return

		case "newBitcoinAddress":
			addr, err := ln.NewAddress()
			if err != nil {
				log.Printf("unable to connect to RPC server: %v", err)
				redirectWithError(w, r, "/bitcoin?", err)
				return
			}

			// Redirect to bitcoin page with new address
			http.Redirect(w, r, "/bitcoin?addr="+addr, http.StatusSeeOther)
			return

		case "newAddress":
			label := r.FormValue("addressLabel")
			addressType := "blech32"
			if r.FormValue("bech32m") == "on" {
				addressType = "bech32m"
			}

			addr, err := liquid.GetNewAddress(label, addressType)
			if err != nil {
				redirectWithError(w, r, "/liquid?", err)
				return
			}

			// Redirect to liquid page with new address
			http.Redirect(w, r, "/liquid?addr="+addr, http.StatusSeeOther)
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

			premiumLimit, _ := strconv.ParseInt(r.FormValue("premiumLimit"), 10, 64)

			var id string
			asset := r.FormValue("from")
			direction := "in"
			if asset == "ln" {
				asset = r.FormValue("to")
				direction = "out"
			}
			if asset == "ln" || r.FormValue("from") != "ln" && r.FormValue("to") != "ln" {
				redirectWithError(w, r, "/peer?id="+nodeId+"&", errors.New("invalid combination of assets"))
				return
			}

			switch direction {
			case "in":
				id, err = ps.SwapIn(client, swapAmount, channelId, asset, false, premiumLimit)
			case "out":
				id, err = ps.SwapOut(client, swapAmount, channelId, asset, false, premiumLimit)
			}

			if err != nil {
				e := err.Error()
				if e == "Request timed out" || strings.Contains(e, "rpc timeout reached") {
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

			// delete peer balance information
			if asset == "btc" {
				if ln.BitcoinBalances != nil {
					delete(ln.BitcoinBalances, nodeId)
				}
			} else {
				if ln.LiquidBalances != nil {
					delete(ln.LiquidBalances, nodeId)
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
				if err := config.Save(); err != nil {
					redirectWithError(w, r, "/config?", err)
					return
				}
				http.Redirect(w, r, "/ca", http.StatusSeeOther)
				return
			}

			if r.FormValue("serverIPs") != config.Config.ServerIPs {
				config.Config.ServerIPs = r.FormValue("serverIPs")
				if secureConnection {
					if err := config.GenerateServerCertificate(); err == nil {
						showRestartScreen(w, r, true, config.Config.Password, true)
					} else {
						log.Println("GenereateServerCertificate:", err)
						redirectWithError(w, r, "/config?", err)
						return
					}
				}
			}

			if !secureConnection && config.Config.SecureConnection {
				// restart to listen on HTTP only
				showRestartScreen(w, r, false, "", true)
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

		// disable broadcasting
		if !allowSwapRequests {
			ln.AdvertiseLiquidBalance = false
			db.Save("Peers", "AdvertiseLiquidBalance", ln.AdvertiseLiquidBalance)
		}

		if !allowSwapRequests || !bitcoinSwaps {
			ln.AdvertiseBitcoinBalance = false
			db.Save("Peers", "AdvertiseBitcoinBalance", ln.AdvertiseBitcoinBalance)
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
			redirectWithError(w, r, "/config?", err)
			return
		}

		if mustRestart {
			// update peerswap config
			config.SavePS()
			if ln.IMPLEMENTATION == "LND" {
				// show progress bar and log
				go http.Redirect(w, r, "/loading", http.StatusSeeOther)
			} else {
				showRestartScreen(w, r, config.Config.SecureConnection, config.Config.Password, false)
			}
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
	if ln.IMPLEMENTATION == "CLN" {
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
	// returns .bak with the name of the wallet
	if fileName, err := liquid.BackupAndZip(); err == nil {
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

	//check for error message to display
	errorMessage := ""
	keys, ok := r.URL.Query()["err"]
	if ok && len(keys[0]) > 0 {
		errorMessage = keys[0]
	}

	logFile := "log"

	keys, ok = r.URL.Query()["log"]
	if ok && len(keys[0]) > 0 {
		logFile = keys[0]
	}

	data := Page{
		Authenticated:  config.Config.SecureConnection && config.Config.Password != "",
		ColorScheme:    config.Config.ColorScheme,
		ErrorMessage:   errorMessage,
		PopUpMessage:   "",
		MempoolFeeRate: mempoolFeeRate,
		LogPosition:    1, // from first line
		LogFile:        logFile,
		Implementation: ln.IMPLEMENTATION,
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

		// limit to 50000 characters
		if startPosition == 1 && length > 50000 {
			logText = logText[length-50000:]
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
			// nothing can be done, let the browser retry
			return
		} else {
			log.Fatalf("Template execution error: %v", err)
		}
	}
}
