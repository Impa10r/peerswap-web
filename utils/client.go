package utils

import (
	"context"
	"fmt"

	"github.com/elementsproject/peerswap/peerswaprpc"
	"google.golang.org/grpc"
)

func AllowSwapRequests(allow bool) error {
	host := Config.RpcHost
	ctx := context.Background()

	client, cleanup, err := GetClient(host)
	if err != nil {
		return err
	}
	defer cleanup()

	_, err = client.AllowSwapRequests(ctx, &peerswaprpc.AllowSwapRequestsRequest{
		Allow: allow,
	})
	if err != nil {
		return err
	}

	return nil
}

func GetClient(rpcServer string) (peerswaprpc.PeerSwapClient, func(), error) {
	conn, err := GetClientConn(rpcServer)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { conn.Close() }

	psClient := peerswaprpc.NewPeerSwapClient(conn)
	return psClient, cleanup, nil
}

func GetClientConn(address string) (*grpc.ClientConn, error) {

	maxMsgRecvSize := grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200)
	opts := []grpc.DialOption{
		grpc.WithDefaultCallOptions(maxMsgRecvSize),
		grpc.WithInsecure(),
	}

	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	return conn, nil
}
