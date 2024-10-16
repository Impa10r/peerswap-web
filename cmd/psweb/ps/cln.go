//go:build cln

package ps

import (
	"fmt"
	"log"
	"peerswap-web/cmd/psweb/config"
	"strconv"
	"strings"

	"github.com/elementsproject/glightning/glightning"
	"github.com/elementsproject/peerswap/clightning"
	"github.com/elementsproject/peerswap/peerswaprpc"
)

const JSON_RPC = "lightning-rpc"

func GetClient(dirRPC string) (*glightning.Lightning, func(), error) {
	lightning := glightning.NewLightning()
	err := lightning.StartUp(JSON_RPC, dirRPC)
	if err != nil {
		log.Println("PS CLN Connection:", err)
		return nil, nil, err
	}

	cleanup := func() {
		// lightning.Shutdown()
	}

	return lightning, cleanup, nil
}

func ReloadPolicyFile(client *glightning.Lightning) (*peerswaprpc.Policy, error) {
	var res map[string]interface{}

	err := client.Request(&clightning.ReloadPolicyFile{}, &res)
	if err != nil {
		return nil, err
	}

	var allowed []string
	var suspected []string

	for _, p := range res["allowlisted_peers"].([]interface{}) {
		allowed = append(allowed, p.(string))
	}

	for _, p := range res["suspicious_peers"].([]interface{}) {
		suspected = append(suspected, p.(string))
	}

	return &peerswaprpc.Policy{
		AllowlistedPeers:   allowed,
		SuspiciousPeerList: suspected,
	}, nil
}

func Stop() {
	log.Println("Stopping lightningd...")
	client, cleanup, err := GetClient(config.Config.RpcHost)
	if err != nil {
		log.Println("Unable to stop lightningd:", err)
		return
	}
	defer cleanup()

	client.Stop()
}

func ListPeers(client *glightning.Lightning) (*peerswaprpc.ListPeersResponse, error) {
	var res []map[string]interface{}

	err := client.Request(&clightning.ListPeers{}, &res)
	if err != nil {
		log.Println("ListPeers:", err)
		return nil, err
	}

	var peers []*peerswaprpc.PeerSwapPeer

	for _, data := range res {
		peer := peerswaprpc.PeerSwapPeer{}
		peer.NodeId = data["nodeid"].(string)
		peer.SwapsAllowed = data["swaps_allowed"].(bool)

		// Check if the "total_fee_paid" field exists
		if totalFeePaid, ok := data["total_fee_paid"]; ok {
			peer.PaidFee = uint64(totalFeePaid.(float64))
		}

		assets := data["supported_assets"].([]interface{})
		for _, asset := range assets {
			peer.SupportedAssets = append(peer.SupportedAssets, asset.(string))
		}

		channels := data["channels"].([]interface{})
		for _, channel := range channels {
			channelData := channel.(map[string]interface{})
			peer.Channels = append(peer.Channels, &peerswaprpc.PeerSwapPeerChannel{
				ChannelId:     convertClnToLndChannelId(channelData["short_channel_id"].(string)),
				LocalBalance:  uint64(channelData["local_balance"].(float64)),
				RemoteBalance: uint64(channelData["remote_balance"].(float64)),
				Active:        channelData["state"].(string) == "CHANNELD_NORMAL",
			})
		}

		asSender := data["sent"].(map[string]interface{})
		peer.AsSender = &peerswaprpc.SwapStats{
			SwapsOut: uint64(asSender["total_swaps_out"].(float64)),
			SwapsIn:  uint64(asSender["total_swaps_in"].(float64)),
			SatsOut:  uint64(asSender["total_sats_swapped_out"].(float64)),
			SatsIn:   uint64(asSender["total_sats_swapped_in"].(float64)),
		}

		asReceiver := data["received"].(map[string]interface{})
		peer.AsReceiver = &peerswaprpc.SwapStats{
			SwapsOut: uint64(asReceiver["total_swaps_out"].(float64)),
			SwapsIn:  uint64(asReceiver["total_swaps_in"].(float64)),
			SatsOut:  uint64(asReceiver["total_sats_swapped_out"].(float64)),
			SatsIn:   uint64(asReceiver["total_sats_swapped_in"].(float64)),
		}

		peers = append(peers, &peer)
	}

	list := peerswaprpc.ListPeersResponse{
		Peers: peers,
	}

	return &list, nil
}

func ListSwaps(client *glightning.Lightning) (*peerswaprpc.ListSwapsResponse, error) {
	var res peerswaprpc.ListSwapsResponse

	err := client.Request(&clightning.ListSwaps{}, &res)
	if err != nil {
		log.Println("ListSwaps:", err)
		return nil, err
	}

	return &res, nil
}

func LiquidGetBalance(client *glightning.Lightning) (*peerswaprpc.GetBalanceResponse, error) {
	var res map[string]interface{}

	err := client.Request(&clightning.LiquidGetBalance{}, &res)
	if err != nil {
		log.Println("LiquidGetBalance:", err)
		return nil, err
	}

	return &peerswaprpc.GetBalanceResponse{
		SatAmount: uint64(res["lbtc_balance_sat"].(float64)),
	}, nil
}

func ListActiveSwaps(client *glightning.Lightning) (*peerswaprpc.ListSwapsResponse, error) {
	var res peerswaprpc.ListSwapsResponse

	err := client.Request(&clightning.ListActiveSwaps{}, &res)
	if err != nil {
		log.Println("ListActiveSwaps:", err)
		return nil, err
	}

	return &res, nil
}

func GetSwap(client *glightning.Lightning, id string) (*peerswaprpc.SwapResponse, error) {
	var res peerswaprpc.ListSwapsResponse

	err := client.Request(&clightning.ListSwaps{}, &res)
	if err != nil {
		log.Println("ListSwaps:", err)
		return nil, err
	}

	for _, swap := range res.GetSwaps() {
		if swap.Id == id {
			return &peerswaprpc.SwapResponse{
				Swap: swap,
			}, nil
		}
	}

	return nil, fmt.Errorf("swap not found")
}

func LiquidGetAddress(client *glightning.Lightning) (*peerswaprpc.GetAddressResponse, error) {
	var res map[string]interface{}

	err := client.Request(&clightning.LiquidGetAddress{}, &res)
	if err != nil {
		log.Println("LiquidGetAddress:", err)
		return nil, err
	}

	return &peerswaprpc.GetAddressResponse{
		Address: res["lbtc_address"].(string),
	}, nil
}

func AddPeer(client *glightning.Lightning, nodeId string) (*peerswaprpc.Policy, error) {
	var res peerswaprpc.Policy

	err := client.Request(&clightning.AddPeer{
		PeerPubkey: nodeId,
	}, &res)
	if err != nil {
		log.Println("AddPeer:", err)
		return nil, err
	}

	return &res, nil
}

func RemovePeer(client *glightning.Lightning, nodeId string) (*peerswaprpc.Policy, error) {
	var res peerswaprpc.Policy

	err := client.Request(&clightning.RemovePeer{
		PeerPubkey: nodeId,
	}, &res)
	if err != nil {
		log.Println("RemovePeer:", err)
		return nil, err
	}

	return &res, nil
}

func AddSusPeer(client *glightning.Lightning, nodeId string) (*peerswaprpc.Policy, error) {
	var res peerswaprpc.Policy

	err := client.Request(&clightning.AddSuspiciousPeer{
		PeerPubkey: nodeId,
	}, &res)
	if err != nil {
		log.Println("AddSuspiciousPeer:", err)
		return nil, err
	}

	return &res, nil
}

func RemoveSusPeer(client *glightning.Lightning, nodeId string) (*peerswaprpc.Policy, error) {
	var res peerswaprpc.Policy

	err := client.Request(&clightning.RemoveSuspiciousPeer{
		PeerPubkey: nodeId,
	}, &res)
	if err != nil {
		log.Println("RemoveSuspiciousPeer:", err)
		return nil, err
	}

	return &res, nil
}

func SwapIn(client *glightning.Lightning, swapAmount, channelId uint64, asset string, force bool, premiumLimit int64) (string, error) {
	var res map[string]interface{}

	err := client.Request(&clightning.SwapIn{
		ShortChannelId: convertLndToClnChannelId(channelId),
		SatAmt:         swapAmount,
		Asset:          asset,
		Force:          force,
		PremiumLimit:   premiumLimit,
	}, &res)

	if err != nil {
		return "", err
	}

	return res["id"].(string), nil
}

func SwapOut(client *glightning.Lightning, swapAmount, channelId uint64, asset string, force bool, premiumLimit int64) (string, error) {
	var res map[string]interface{}

	err := client.Request(&clightning.SwapOut{
		ShortChannelId: convertLndToClnChannelId(channelId),
		SatAmt:         swapAmount,
		Asset:          asset,
		Force:          force,
		PremiumLimit:   premiumLimit,
	}, &res)

	if err != nil {
		return "", err
	}

	return res["id"].(string), nil
}

func AllowSwapRequests(client *glightning.Lightning, allowSwapRequests bool) (*peerswaprpc.Policy, error) {
	var res map[string]interface{}
	allow := "0"
	if allowSwapRequests {
		allow = "1"
	}

	err := client.Request(&clightning.AllowSwapRequests{
		AllowSwapRequestsString: allow,
	}, &res)
	if err != nil {
		log.Println("allowSwapRequests:", err)
		return nil, err
	}

	// return empty object as it is not used
	return &peerswaprpc.Policy{}, nil
}

// convert short channel id 2568777x70x1 to LND format
func convertClnToLndChannelId(s string) uint64 {
	parts := strings.Split(s, "x")
	if len(parts) != 3 {
		return 0 // or handle error appropriately
	}

	var scid uint64
	for i, part := range parts {
		val, err := strconv.Atoi(part)
		if err != nil {
			return 0 // or handle error appropriately
		}
		switch i {
		case 0:
			scid |= uint64(val) << 40
		case 1:
			scid |= uint64(val) << 16
		case 2:
			scid |= uint64(val)
		}
	}
	return scid
}

// convert LND channel id to CLN 2568777x70x1
func convertLndToClnChannelId(s uint64) string {
	block := strconv.FormatUint(s>>40, 10)
	tx := strconv.FormatUint((s>>16)&0xFFFFFF, 10)
	output := strconv.FormatUint(s&0xFFFF, 10)
	return block + "x" + tx + "x" + output
}
