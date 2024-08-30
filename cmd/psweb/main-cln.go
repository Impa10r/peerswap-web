//go:build cln

package main

import (
	"encoding/binary"
	"encoding/hex"
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
	plugin = glightning.NewPlugin(onInit)
	registerHooks(plugin)

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

func registerHooks(p *glightning.Plugin) {
	p.RegisterHooks(&glightning.Hooks{
		CustomMsgReceived: onCustomMsgReceived,
	})
}

func onCustomMsgReceived(event *glightning.CustomMsgReceivedEvent) (*glightning.CustomMsgReceivedResponse, error) {
	typeBytes, err := hex.DecodeString(event.Payload[:4])
	if err == nil {
		if binary.BigEndian.Uint16(typeBytes) == ln.MESSAGE_TYPE {
			payload, err := hex.DecodeString(event.Payload[4:])
			if err == nil {
				ln.OnMyCustomMessage(event.PeerId, payload)
			} else {
				log.Println()
			}
		}
	}

	return &glightning.CustomMsgReceivedResponse{
		Result: "continue",
	}, nil
}
