//go:build clnversion

package ps

import (
	"log"
	"peerswap-web/cmd/psweb/config"

	"github.com/elementsproject/glightning/glightning"
	"github.com/elementsproject/peerswap/clightning"
	"github.com/elementsproject/peerswap/peerswaprpc"
)

const fileRPC = "lightning-rpc"

func getClient(dirRPC string) (*glightning.Lightning, func(), error) {
	lightning := glightning.NewLightning()
	err := lightning.StartUp(fileRPC, dirRPC)
	if err != nil {
		log.Println("PS CLN Connection:", err)
		return nil, nil, err
	}

	cleanup := func() { lightning.Shutdown() }

	return lightning, cleanup, nil
}

func ReloadPolicyFile() (*peerswaprpc.Policy, error) {
	client, cleanup, err := getClient(config.Config.RpcHost)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	var res peerswaprpc.Policy

	err = client.Request(&clightning.ReloadPolicyFile{}, &res)
	if err != nil {
		log.Println("ReloadPolicyFile:", err)
		return nil, err
	}

	return &res, nil
}

/*
	func Stop() {
		client, cleanup, err := getClient(config.Config.RpcHost)

		if err != nil {
			return
		}

		defer cleanup()

		log.Println("Stopping lightningd...")

		_, err = client.Stop()

		if err != nil {
			log.Println("Unable to stop lightningd:", err)
		}
	}

	func ListPeers() (*peerswaprpc.ListPeersResponse, error) {
		client, cleanup, err := getClient(config.Config.RpcHost)
		if err != nil {
			return nil, err
		}
		defer cleanup()

		var res peerswaprpc.ListPeersResponse

		err = client.Request(&clightning.ListPeers{}, &res)
		if err != nil {
			log.Println("ListPeers:", err)
			return nil, err
		}

		return &res, nil
	}

	func ListSwaps() (*peerswaprpc.ListSwapsResponse, error) {
		client, cleanup, err := getClient(config.Config.RpcHost)
		if err != nil {
			return nil, err
		}
		defer cleanup()

		var res peerswaprpc.ListSwapsResponse

		err = client.Request(&clightning.ListSwaps{}, &res)
		if err != nil {
			log.Println("ListSwaps:", err)
			return nil, err
		}

		return &res, nil
	}

	func LiquidGetBalance() (*peerswaprpc.GetBalanceResponse, error) {
		client, cleanup, err := getClient(config.Config.RpcHost)
		if err != nil {
			return nil, err
		}
		defer cleanup()

		var res peerswaprpc.GetBalanceResponse

		err = client.Request(&clightning.LiquidGetBalance{}, &res)
		if err != nil {
			log.Println("LiquidGetBalance:", err)
			return nil, err
		}

		return &res, nil
	}

	func ListActiveSwaps() (*peerswaprpc.ListSwapsResponse, error) {
		client, cleanup, err := getClient(config.Config.RpcHost)
		if err != nil {
			return nil, err
		}
		defer cleanup()

		var res peerswaprpc.ListSwapsResponse

		err = client.Request(&clightning.ListActiveSwaps{}, &res)
		if err != nil {
			log.Println("ListActiveSwaps:", err)
			return nil, err
		}

		return &res, nil
	}

	func GetSwap(id string) (*peerswaprpc.SwapResponse, error) {
		client, cleanup, err := getClient(config.Config.RpcHost)
		if err != nil {
			return nil, err
		}
		defer cleanup()

		var res peerswaprpc.SwapResponse

		err = client.Request(&clightning.GetSwap{
			SwapId: id,
		}, &res)
		if err != nil {
			log.Println("GetSwap:", err)
			return nil, err
		}

		return &res, nil
	}

	func LiquidGetAddress() (*peerswaprpc.GetAddressResponse, error) {
		client, cleanup, err := getClient(config.Config.RpcHost)
		if err != nil {
			return nil, err
		}
		defer cleanup()

		var res peerswaprpc.GetAddressResponse

		err = client.Request(&clightning.LiquidGetAddress{}, &res)
		if err != nil {
			log.Println("LiquidGetAddress:", err)
			return nil, err
		}

		return &res, nil
	}

	func AddPeer(nodeId string) (*peerswaprpc.Policy, error) {
		client, cleanup, err := getClient(config.Config.RpcHost)
		if err != nil {
			return nil, err
		}
		defer cleanup()

		var res peerswaprpc.Policy

		err = client.Request(&clightning.AddPeer{
			PeerPubkey: nodeId
		}, &res)
		if err != nil {
			log.Println("AddPeer:", err)
			return nil, err
		}

		return &res, nil
	}

	func RemovePeer(nodeId string) (*peerswaprpc.Policy, error) {
		client, cleanup, err := getClient(config.Config.RpcHost)
		if err != nil {
			return nil, err
		}
		defer cleanup()

		var res peerswaprpc.Policy

		err = client.Request(&clightning.RemovePeer{
			PeerPubkey: nodeId
		}, &res)
		if err != nil {
			log.Println("RemovePeer:", err)
			return nil, err
		}

		return &res, nil
	}

	func AddSusPeer(nodeId string) (*peerswaprpc.Policy, error) {
		client, cleanup, err := getClient(config.Config.RpcHost)
		if err != nil {
			return nil, err
		}
		defer cleanup()

		var res peerswaprpc.Policy

		err = client.Request(&clightning.AddSuspiciousPeer{
			PeerPubkey: nodeId
		}, &res)
		if err != nil {
			log.Println("AddSuspiciousPeer:", err)
			return nil, err
		}

		return &res, nil
	}

	func RemoveSusPeer(nodeId string) (*peerswaprpc.Policy, error) {
		client, cleanup, err := getClient(config.Config.RpcHost)
		if err != nil {
			return nil, err
		}
		defer cleanup()

		var res peerswaprpc.Policy

		err = client.Request(&clightning.RemoveSuspiciousPeer{
			PeerPubkey: nodeId
		}, &res)
		if err != nil {
			log.Println("RemoveSuspiciousPeer:", err)
			return nil, err
		}

		return &res, nil
	}

	func SwapIn(swapAmount, channelId uint64, asset string, force bool) (*peerswaprpc.SwapResponse, error) {
		client, cleanup, err := getClient(config.Config.RpcHost)
		if err != nil {
			return nil, err
		}
		defer cleanup()

		var res peerswaprpc.SwapResponse

		err = client.Request(&clightning.SwapIn{
			ShortChannelId: strconv.FormatUint(channelId, 10),
			SatAmt: swapAmount,
			Asset: asset,
			Force: force,

		}, &res)

		if err != nil {
			log.Println("SwapIn:", err)
			return nil, err
		}

		return &res, nil
	}

	func SwapOut(swapAmount, channelId uint64, asset string, force bool) (*peerswaprpc.SwapResponse, error) {
		client, cleanup, err := getClient(config.Config.RpcHost)
		if err != nil {
			return nil, err
		}
		defer cleanup()

		var res peerswaprpc.SwapResponse

		err = client.Request(&clightning.SwapOut{
			ShortChannelId: strconv.FormatUint(channelId, 10),
			SatAmt: swapAmount,
			Asset: asset,
			Force: force,

		}, &res)

		if err != nil {
			log.Println("SwapOut:", err)
			return nil, err
		}

		return &res, nil
	}

	func AllowSwapRequests(host string, allowSwapRequests bool) (*peerswaprpc.Policy, error) {
		client, cleanup, err := getClient(host)
		if err != nil {
			return nil, err
		}
		defer cleanup()

		var res peerswaprpc.Policy
		allow := "0"
		if allowSwapRequests {
	        allow =  "1"
	    }

		err = client.Request(&clightning.AllowSwapRequests{
			AllowSwapRequestsString: allow,
		}, &res)
		if err != nil {
			log.Println("allowSwapRequests:", err)
			return nil, err
		}

		return &res, nil
	}
*/

func Stop() {
}

func ListPeers() (*peerswaprpc.ListPeersResponse, error) {
	return nil, nil
}

func ListSwaps() (*peerswaprpc.ListSwapsResponse, error) {
	return nil, nil
}

func LiquidGetBalance() (*peerswaprpc.GetBalanceResponse, error) {
	return nil, nil
}

func ListActiveSwaps() (*peerswaprpc.ListSwapsResponse, error) {
	return nil, nil
}

func GetSwap(id string) (*peerswaprpc.SwapResponse, error) {
	return nil, nil
}

func LiquidGetAddress() (*peerswaprpc.GetAddressResponse, error) {
	return nil, nil
}

func AddPeer(nodeId string) (*peerswaprpc.Policy, error) {
	return nil, nil
}

func RemovePeer(nodeId string) (*peerswaprpc.Policy, error) {
	return nil, nil
}

func AddSusPeer(nodeId string) (*peerswaprpc.Policy, error) {
	return nil, nil
}

func RemoveSusPeer(nodeId string) (*peerswaprpc.Policy, error) {
	return nil, nil
}

func SwapIn(swapAmount, channelId uint64, asset string, force bool) (*peerswaprpc.SwapResponse, error) {
	return nil, nil
}

func SwapOut(swapAmount, channelId uint64, asset string, force bool) (*peerswaprpc.SwapResponse, error) {
	return nil, nil
}

func AllowSwapRequests(host string, allowSwapRequests bool) (*peerswaprpc.Policy, error) {
	return nil, nil
}
