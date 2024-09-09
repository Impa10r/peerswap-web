//go:build !cln

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"peerswap-web/cmd/psweb/config"
	"syscall"
)

func main() {
	var (
		dataDir     = flag.String("datadir", "", "Path to peerswap data folder")
		password    = flag.String("password", "", "Run with HTTPS password authentication")
		showHelp    = flag.Bool("help", false, "Show help")
		showVersion = flag.Bool("version", false, "Show version")
	)

	flag.Parse()

	if *showHelp {
		fmt.Println("A lightweight Web UI for PeerSwap LND")
		fmt.Println("Usage:")
		flag.PrintDefaults()
		os.Exit(0)
	}

	if *showVersion {
		fmt.Printf("PeerSwap Web UI %s for LND", VERSION)
		os.Exit(0)
	}

	// loading from config file or creating default one
	config.Load(*dataDir, os.Getenv("NETWORK"))

	if *password != "" {
		// enable HTTPS
		config.Config.SecureConnection = true
		config.Config.Password = *password
		config.Save()
	}

	// set logging params
	cleanup, err := setLogging(filepath.Join(config.Config.DataDir, "psweb.log"))
	if err != nil {
		log.Fatal(err)
	}
	defer cleanup()

	// start the web server
	start()

	// Handle termination signals
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	// Wait for termination signal
	sig := <-signalChan
	log.Printf("Received termination signal: %s\n", sig)

	// Exit the program gracefully
	os.Exit(0)
}
