package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"log"

	"github.com/elementsproject/peerswap/peerswaprpc"
	"google.golang.org/grpc"
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
		grpc.WithInsecure(),
	}

	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v",
			err)
	}

	return conn, nil
}

func isRunningPeerSwapd() bool {

	host := config.RpcHost
	ctx := context.Background()

	client, cleanup, err := getClient(host)
	if err != nil {
		log.Println(fmt.Errorf("unable to connect to RPC server: %v", err))
		return false
	}
	defer cleanup()

	// this method will fail if peerswapd is not running or misconfigured
	_, err = client.ReloadPolicyFile(ctx, &peerswaprpc.ReloadPolicyFileRequest{})
	return err == nil
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
		return
	}
}

func launchService() {
	createService()
	cmd := exec.Command("/usr/bin/systemctl start peerswapd", "")
	log.Println("Launching peerswapd service...")
	if err := cmd.Run(); err != nil {
		log.Println("Error:", err)
	} else {
		log.Println("Launched peerswapd service")
		wasLaunched = true
	}
}

func createService() {

	filename := "/etc/systemd/system/peerswapd.service"

	t := "[Service]"
	t += "ExecStart=/root/peerswapd"
	t += "User=root"
	t += "Type=simple"
	t += "KillMode=process"
	t += "TimeoutSec=180"
	t += "Restart=always"
	t += "RestartSec=1"
	//t += "StandardOutput=append:/root/.peerswap/peerswapd.log"
	//t += "StandardError=append:/root/.peerswap/peerswapd.log"
	t += "[Install]"
	t += "WantedBy=multi-user.target"

	data := []byte(t)

	// Open the file in write-only mode, truncate if exists or create a new file
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0644)
	if err != nil {
		log.Println("Error opening file:", err)
		return
	}
	defer file.Close()

	// Write data to the file
	_, err = file.Write(data)
	if err != nil {
		log.Println("Error writing to file:", err)
		return
	}
}
