//go:build !clnversion

package ps

import (
	"context"

	"log"

	"peerswap-web/cmd/psweb/config"

	"github.com/elementsproject/peerswap/peerswaprpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func getClient(rpcServer string) (peerswaprpc.PeerSwapClient, func(), error) {
	conn, err := getClientConn(rpcServer)
	if err != nil {
		log.Println("PS LND Connection:", err)
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
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}

	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

func Stop() {
	host := config.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		return
	}
	defer cleanup()

	log.Println("Stopping peerswapd...")

	_, err = client.Stop(ctx, &peerswaprpc.Empty{})
	if err != nil {
		log.Printf("Unable to stop peerswapd:", err)
	}
}

func ReloadPolicyFile() (*peerswaprpc.Policy, error) {
	host := config.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return client.ReloadPolicyFile(ctx, &peerswaprpc.ReloadPolicyFileRequest{})
}

func ListPeers() (*peerswaprpc.ListPeersResponse, error) {
	host := config.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return client.ListPeers(ctx, &peerswaprpc.ListPeersRequest{})
}

func ListSwaps() (*peerswaprpc.ListSwapsResponse, error) {
	host := config.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return client.ListSwaps(ctx, &peerswaprpc.ListSwapsRequest{})
}

func LiquidGetBalance() (*peerswaprpc.GetBalanceResponse, error) {
	host := config.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return client.LiquidGetBalance(ctx, &peerswaprpc.GetBalanceRequest{})
}

func ListActiveSwaps() (*peerswaprpc.ListSwapsResponse, error) {
	host := config.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return client.ListActiveSwaps(ctx, &peerswaprpc.ListSwapsRequest{})
}

func GetSwap(id string) (*peerswaprpc.SwapResponse, error) {
	host := config.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return client.GetSwap(ctx, &peerswaprpc.GetSwapRequest{
		SwapId: id,
	})
}

func LiquidGetAddress() (*peerswaprpc.GetAddressResponse, error) {
	host := config.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return client.LiquidGetAddress(ctx, &peerswaprpc.GetAddressRequest{})
}

func AddPeer(nodeId string) (*peerswaprpc.Policy, error) {
	host := config.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return client.AddPeer(ctx, &peerswaprpc.AddPeerRequest{
		PeerPubkey: nodeId,
	})
}

func RemovePeer(nodeId string) (*peerswaprpc.Policy, error) {
	host := config.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return client.RemovePeer(ctx, &peerswaprpc.RemovePeerRequest{
		PeerPubkey: nodeId,
	})
}

func AddSusPeer(nodeId string) (*peerswaprpc.Policy, error) {
	host := config.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return client.AddSusPeer(ctx, &peerswaprpc.AddPeerRequest{
		PeerPubkey: nodeId,
	})
}

func RemoveSusPeer(nodeId string) (*peerswaprpc.Policy, error) {
	host := config.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return client.RemoveSusPeer(ctx, &peerswaprpc.RemovePeerRequest{
		PeerPubkey: nodeId,
	})
}

func SwapIn(swapAmount, channelId uint64, asset string, force bool) (*peerswaprpc.SwapResponse, error) {
	host := config.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return client.SwapIn(ctx, &peerswaprpc.SwapInRequest{
		SwapAmount: swapAmount,
		ChannelId:  channelId,
		Asset:      asset,
		Force:      force,
	})
}

func SwapOut(swapAmount, channelId uint64, asset string, force bool) (*peerswaprpc.SwapResponse, error) {
	host := config.Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return client.SwapOut(ctx, &peerswaprpc.SwapOutRequest{
		SwapAmount: swapAmount,
		ChannelId:  channelId,
		Asset:      asset,
		Force:      force,
	})
}

func AllowSwapRequests(host string, allowSwapRequests bool) (*peerswaprpc.Policy, error) {
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	return client.AllowSwapRequests(ctx, &peerswaprpc.AllowSwapRequestsRequest{
		Allow: allowSwapRequests,
	})
}
