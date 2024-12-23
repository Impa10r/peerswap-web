//go:build cln

package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/ln"

	"github.com/elementsproject/glightning/glightning"
	"github.com/virtuald/go-paniclog"
)

var plugin *glightning.Plugin

// This is called after the plugin starts up successfully
func onInit(plugin *glightning.Plugin, options map[string]glightning.Option, conf *glightning.Config) {
	// set logging params
	_, err := setLogging(filepath.Join(conf.LightningDir, "peerswap", "psweb.log"))
	if err != nil {
		log.Fatal(err)
	}

	// loading from config file or creating default one
	config.Load(filepath.Dir(conf.LightningDir), conf.Network)

	// redirect stderr to our log file, so that we can see panics
	if err := redirectStderr(filepath.Join(config.Config.DataDir, "psweb.log")); err != nil {
		log.Fatalln(err)
	}

	// start the web server
	start()
}

func main() {
	var (
		showHelp    = flag.Bool("help", false, "Show help")
		showVersion = flag.Bool("version", false, "Show version")
		developer   = flag.Bool("developer", false, "Flag passed by clightningd")
	)

	flag.Parse()

	if *showHelp {
		fmt.Println("A lightweight Web UI plugin for PeerSwap CLN")
		fmt.Println("Usage: add 'plugin=/path/to/psweb' to your ~/.lightning/config")
		os.Exit(0)
	}

	if *showVersion {
		fmt.Printf("PeerSwap Web UI %s for CLN\n", VERSION)
		os.Exit(0)
	}

	if *developer {
		debug = true
	}

	plugin = glightning.NewPlugin(onInit)

	plugin.RegisterHooks(&glightning.Hooks{
		CustomMsgReceived: onCustomMsgReceived,
	})

	plugin.SubscribeSendPaySuccess(onSendPaySuccess)
	plugin.SubscribeInvoicePaid(onInvoicePaid)

	err := plugin.Start(os.Stdin, os.Stdout)
	if err != nil {
		log.Fatalln(err)
	}
}

func redirectStderr(filename string) error {
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	_, err = paniclog.RedirectStderr(f)
	if err != nil {
		return err
	}
	return nil
}

func onCustomMsgReceived(event *glightning.CustomMsgReceivedEvent) (*glightning.CustomMsgReceivedResponse, error) {
	typeBytes, err := hex.DecodeString(event.Payload[:4])
	if err == nil {
		if binary.BigEndian.Uint16(typeBytes) == ln.MESSAGE_TYPE {
			payload, err := hex.DecodeString(event.Payload[4:])
			if err == nil {
				ln.OnMyCustomMessage(event.PeerId, payload)
			} else {
				log.Println("Cannot decode my custom message:", err)
			}
		}
	}

	return event.Continue(), nil
}

func onSendPaySuccess(ss *glightning.SendPaySuccess) {
	go func(paymentHash string) {
		time.Sleep(10 * time.Second) // wait to allow htlcs to settle
		ln.CacheHTLCs("payment_hash=x'" + paymentHash + "'")
	}(ss.PaymentHash)
}

func onInvoicePaid(p *glightning.Payment) {
	// Convert the hex string to bytes
	preimage, err := hex.DecodeString(p.PreImage)
	if err != nil {
		log.Println("Error decoding preimage:", err)
		return
	}

	// Compute the SHA-256 hash of the preimage
	hash := sha256.Sum256(preimage)

	// Convert the hash to a hex string
	hashHex := hex.EncodeToString(hash[:])

	// fetch and cache HTLCs by PaymentHash
	ln.CacheHTLCs("payment_hash=x'" + hashHex + "'")
}
