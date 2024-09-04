//go:build cln

package main

import (
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/ln"

	"github.com/elementsproject/glightning/glightning"
	"github.com/virtuald/go-paniclog"
)

var plugin *glightning.Plugin

// This is called after the plugin starts up successfully
func onInit(plugin *glightning.Plugin, options map[string]glightning.Option, conf *glightning.Config) {

	// loading from config file or creating default one
	config.Load(filepath.Dir(conf.LightningDir), conf.Network)

	// redirect stderr to our log file, so that we can see panics
	if err := redirectStderr(filepath.Join(config.Config.DataDir, "psweb.log")); err != nil {
		log.Fatalln(err)
	}

	// set logging params
	_, err := setLogging()
	if err != nil {
		log.Fatal(err)
	}
	//defer cleanup()

	// start the web server
	start()
}

func main() {
	var (
		showHelp    = flag.Bool("help", false, "Show help")
		showVersion = flag.Bool("version", false, "Show version")
	)

	flag.Parse()

	if *showHelp {
		fmt.Println("A lightweight Web UI plugin for PeerSwap CLN")
		fmt.Println("Usage: add 'plugin=/path/to/psweb' to your ~/lightning/config")
		os.Exit(0)
	}

	if *showVersion {
		fmt.Printf("PeerSwap Web UI %s for CLN", version)
		os.Exit(0)
	}

	plugin = glightning.NewPlugin(onInit)

	plugin.RegisterHooks(&glightning.Hooks{
		CustomMsgReceived: onCustomMsgReceived,
		HtlcAccepted:      onHtlcAccepted,
	})

	plugin.SubscribeSendPaySuccess(onSendPaySuccess)

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
	/*	client, clean, err := ln.GetClient()
		if err != nil {
			return false
		}
		defer clean()

		pmt, err := client.ListSendPaysByHash(ss.PaymentHash)
		if err == nil {
			if len(pmt) == 1 {
				if pmt[0].CompletedAt > timeStamp {
					// returns true if peerswap related
					if !ln.DecodeAndProcessInvoice(pmt[0].Bolt11, int64(pmt.MilliSatoshiSent.msat)) {
					}
				}
			}
		}

		if ss.Destination == ln.MyNodeId {
			// circular rebalancing
			ln.
		}
	*/
	log.Println("onSendPaySuccess:", *ss)
}

func onHtlcAccepted(event *glightning.HtlcAcceptedEvent) (*glightning.HtlcAcceptedResponse, error) {
	log.Println("onHtlcAccepted:", *event)
	return event.Continue(), nil
}