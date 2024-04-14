//go:build !cln

package ps

import (
	"context"

	"log"

	"peerswap-web/cmd/psweb/config"

	"github.com/elementsproject/peerswap/peerswaprpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func GetClient(rpcServer string) (peerswaprpc.PeerSwapClient, func(), error) {
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

	client, cleanup, err := GetClient(host)
	if err != nil {
		return
	}
	defer cleanup()

	log.Println("Stopping peerswapd...")

	_, err = client.Stop(ctx, &peerswaprpc.Empty{})
	if err != nil {
		log.Println("Unable to stop peerswapd:", err)
	} else {
		log.Println("Stopped peerswapd.")
	}

}

func ReloadPolicyFile(client peerswaprpc.PeerSwapClient) (*peerswaprpc.Policy, error) {
	ctx := context.Background()
	return client.ReloadPolicyFile(ctx, &peerswaprpc.ReloadPolicyFileRequest{})
}

func ListPeers(client peerswaprpc.PeerSwapClient) (*peerswaprpc.ListPeersResponse, error) {
	ctx := context.Background()
	return client.ListPeers(ctx, &peerswaprpc.ListPeersRequest{})
}

func ListSwaps(client peerswaprpc.PeerSwapClient) (*peerswaprpc.ListSwapsResponse, error) {
	ctx := context.Background()
	return client.ListSwaps(ctx, &peerswaprpc.ListSwapsRequest{})
}

func LiquidGetBalance(client peerswaprpc.PeerSwapClient) (*peerswaprpc.GetBalanceResponse, error) {
	ctx := context.Background()
	return client.LiquidGetBalance(ctx, &peerswaprpc.GetBalanceRequest{})
}

func ListActiveSwaps(client peerswaprpc.PeerSwapClient) (*peerswaprpc.ListSwapsResponse, error) {
	ctx := context.Background()
	return client.ListActiveSwaps(ctx, &peerswaprpc.ListSwapsRequest{})
}

func GetSwap(client peerswaprpc.PeerSwapClient, id string) (*peerswaprpc.SwapResponse, error) {
	ctx := context.Background()
	return client.GetSwap(ctx, &peerswaprpc.GetSwapRequest{
		SwapId: id,
	})
}

func LiquidGetAddress(client peerswaprpc.PeerSwapClient) (*peerswaprpc.GetAddressResponse, error) {
	ctx := context.Background()
	return client.LiquidGetAddress(ctx, &peerswaprpc.GetAddressRequest{})
}

func AddPeer(client peerswaprpc.PeerSwapClient, nodeId string) (*peerswaprpc.Policy, error) {
	ctx := context.Background()
	return client.AddPeer(ctx, &peerswaprpc.AddPeerRequest{
		PeerPubkey: nodeId,
	})
}

func RemovePeer(client peerswaprpc.PeerSwapClient, nodeId string) (*peerswaprpc.Policy, error) {
	ctx := context.Background()
	return client.RemovePeer(ctx, &peerswaprpc.RemovePeerRequest{
		PeerPubkey: nodeId,
	})
}

func AddSusPeer(client peerswaprpc.PeerSwapClient, nodeId string) (*peerswaprpc.Policy, error) {
	ctx := context.Background()
	return client.AddSusPeer(ctx, &peerswaprpc.AddPeerRequest{
		PeerPubkey: nodeId,
	})
}

func RemoveSusPeer(client peerswaprpc.PeerSwapClient, nodeId string) (*peerswaprpc.Policy, error) {
	ctx := context.Background()
	return client.RemoveSusPeer(ctx, &peerswaprpc.RemovePeerRequest{
		PeerPubkey: nodeId,
	})
}

func SwapIn(client peerswaprpc.PeerSwapClient, swapAmount, channelId uint64, asset string, force bool) (string, error) {
	ctx := context.Background()
	resp, err := client.SwapIn(ctx, &peerswaprpc.SwapInRequest{
		SwapAmount: swapAmount,
		ChannelId:  channelId,
		Asset:      asset,
		Force:      force,
	})

	if err == nil {
		return resp.Swap.Id, nil
	} else {
		return "", err
	}
}

func SwapOut(client peerswaprpc.PeerSwapClient, swapAmount, channelId uint64, asset string, force bool) (string, error) {
	ctx := context.Background()
	resp, err := client.SwapOut(ctx, &peerswaprpc.SwapOutRequest{
		SwapAmount: swapAmount,
		ChannelId:  channelId,
		Asset:      asset,
		Force:      force,
	})

	if err == nil {
		return resp.Swap.Id, nil
	} else {
		return "", err
	}
}

func AllowSwapRequests(client peerswaprpc.PeerSwapClient, allowSwapRequests bool) (*peerswaprpc.Policy, error) {
	ctx := context.Background()
	return client.AllowSwapRequests(ctx, &peerswaprpc.AllowSwapRequestsRequest{
		Allow: allowSwapRequests,
	})
}
