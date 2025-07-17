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
	var res peerswaprpc.Policy

	err := client.Request(&clightning.ReloadPolicyFile{}, &res)
	if err != nil {
		return nil, err
	}

	return &res, nil
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
	var peers []*peerswaprpc.PeerSwapPeer

	err := client.Request(&clightning.ListPeers{}, &peers)
	if err != nil {
		log.Println("ListPeers:", err)
		return nil, err
	}

	return &peerswaprpc.ListPeersResponse{
		Peers: peers,
	}, nil
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
	var res peerswaprpc.GetBalanceResponse

	err := client.Request(&clightning.LiquidGetBalance{}, &res)
	if err != nil {
		log.Println("LiquidGetBalance:", err)
		return nil, err
	}

	return &res, nil
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
	var res peerswaprpc.GetAddressResponse

	err := client.Request(&clightning.LiquidGetAddress{}, &res)
	if err != nil {
		log.Println("LiquidGetAddress:", err)
		return nil, err
	}

	return &res, nil
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
		ShortChannelId:      convertLndToClnChannelId(channelId),
		SatAmt:              swapAmount,
		Asset:               asset,
		Force:               force,
		PremiumLimitRatePPM: premiumLimit,
	}, &res)

	if err != nil {
		return "", err
	}

	return res["id"].(string), nil
}

func SwapOut(client *glightning.Lightning, swapAmount, channelId uint64, asset string, force bool, premiumLimit int64) (string, error) {
	var res map[string]interface{}

	err := client.Request(&clightning.SwapOut{
		ShortChannelId:      convertLndToClnChannelId(channelId),
		SatAmt:              swapAmount,
		Asset:               asset,
		Force:               force,
		PremiumLimitRatePPM: premiumLimit,
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

func UpdateGlobalPremiumRate(
	client *glightning.Lightning,
	rate *peerswaprpc.PremiumRate) (*peerswaprpc.PremiumRate, error) {

	var res peerswaprpc.PremiumRate

	err := client.Request(&clightning.UpdateGlobalPremiumRate{
		Asset:          rate.Asset.String(),
		Operation:      rate.Operation.String(),
		PremiumRatePPM: rate.PremiumRatePpm,
	}, &res)
	if err != nil {
		log.Println("UpdateGlobalPremiumRate:", err)
		return nil, err
	}

	return &res, nil
}

func UpdatePremiumRate(
	client *glightning.Lightning,
	peerNodeId string,
	rate *peerswaprpc.PremiumRate) (*peerswaprpc.PremiumRate, error) {

	var res peerswaprpc.PremiumRate

	err := client.Request(&clightning.UpdatePremiumRate{
		PeerID:         peerNodeId,
		Asset:          rate.Asset.String(),
		Operation:      rate.Operation.String(),
		PremiumRatePPM: rate.PremiumRatePpm,
	}, &res)
	if err != nil {
		log.Println("UpdatePremiumRate:", err)
		return nil, err
	}

	return &res, nil
}

func GetGlobalPremiumRate(
	client *glightning.Lightning,
	asset peerswaprpc.AssetType,
	operation peerswaprpc.OperationType) (*peerswaprpc.PremiumRate, error) {

	var res peerswaprpc.PremiumRate

	err := client.Request(&clightning.GetGlobalPremiumRate{
		Asset:     asset.String(),
		Operation: operation.String(),
	}, &res)
	if err != nil {
		log.Println("GetGlobalPremiumRate:", err)
		return nil, err
	}

	return &res, nil
}

func GetPremiumRate(
	client *glightning.Lightning,
	peerNodeId string,
	asset peerswaprpc.AssetType,
	operation peerswaprpc.OperationType) (*peerswaprpc.PremiumRate, error) {

	var res peerswaprpc.PremiumRate

	err := client.Request(&clightning.GetPremiumRate{
		PeerID:    peerNodeId,
		Asset:     asset.String(),
		Operation: operation.String(),
	}, &res)
	if err != nil {
		log.Println("GetPremiumRate:", err)
		return nil, err
	}

	return &res, nil
}

func DeletePremiumRate(
	client *glightning.Lightning,
	peerNodeId string,
	rate *peerswaprpc.PremiumRate) (*peerswaprpc.PremiumRate, error) {

	var res peerswaprpc.PremiumRate

	err := client.Request(&clightning.DeletePremiumRate{
		PeerID:    peerNodeId,
		Asset:     rate.Asset.String(),
		Operation: rate.Operation.String(),
	}, &res)
	if err != nil {
		log.Println("DeletePremiumRate:", err)
		return nil, err
	}

	return &res, nil
}
