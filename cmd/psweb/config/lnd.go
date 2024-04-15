//go:build !cln

package config

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config defaults for LND
func loadDefaults(home, dataDir string) {
	Config.Implementation = "LND"
	Config.LightningDir = filepath.Join(home, ".lnd")
	Config.RpcHost = "localhost:42069"

	if dataDir == "" {
		Config.DataDir = filepath.Join(home, ".peerswap")
	} else {
		Config.DataDir = dataDir
	}

	host := getLndConfSetting("bitcoind.rpchost")

	if host == "" {
		Config.BitcoinHost = GetBlockIoHost()
		Config.BitcoinUser = ""
		Config.BitcoinPass = ""
	} else {
		port := "8332"
		if Config.Chain == "testnet" {
			port = "18332"
		}
		Config.BitcoinHost = "http://" + host + ":" + port
		Config.BitcoinUser = getLndConfSetting("bitcoind.rpcuser")
		Config.BitcoinPass = getLndConfSetting("bitcoind.rpcpass")
	}
}

// LND-specific load from Peerswap config
func LoadPS() {
	wallet := GetPeerswapLNDSetting("elementsd.rpcwallet")
	if wallet != "" {
		Config.ElementsWallet = wallet
	}

	host := GetPeerswapLNDSetting("elementsd.rpchost")
	if host != "" {
		Config.ElementsHost = host
	}

	port := GetPeerswapLNDSetting("elementsd.rpcport")
	if host != "" {
		Config.ElementsPort = port
	}

	// on first start without config there will be no elements user and password
	if Config.ElementsPass == "" || Config.ElementsUser == "" {
		// check in peerswap.conf
		Config.ElementsPass = GetPeerswapLNDSetting("elementsd.rpcpass")
		Config.ElementsUser = GetPeerswapLNDSetting("elementsd.rpcuser")

		// check if they were passed as env
		if Config.ElementsUser == "" && os.Getenv("ELEMENTS_USER") != "" {
			Config.ElementsUser = os.Getenv("ELEMENTS_USER")
		}
		if Config.ElementsPass == "" && os.Getenv("ELEMENTS_PASS") != "" {
			Config.ElementsPass = os.Getenv("ELEMENTS_PASS")
		}

		// if ElementPass is still empty, this will create temporary peerswap.conf with Liquid disabled
		SavePS()
	}
}

// saves PeerSwapd config to peerswap.conf
func SavePS() {
	filename := filepath.Join(Config.DataDir, "peerswap.conf")

	t := "# Config managed by PeerSwap Web UI\n"
	t += "# It is not recommended to modify this file directly\n\n"

	//key, default, new value, env key
	t += setPeerswapdVariable("host", "localhost:42069", Config.RpcHost, "")
	t += setPeerswapdVariable("rpchost", "localhost:42070", "", "")
	t += setPeerswapdVariable("lnd.host", "localhost:10009", "", "LND_HOST")
	t += setPeerswapdVariable("lnd.tlscertpath", filepath.Join(Config.LightningDir, "tls.cert"), "", "")
	t += setPeerswapdVariable("lnd.macaroonpath", filepath.Join(Config.LightningDir, "data", "chain", "bitcoin", Config.Chain, "admin.macaroon"), "", "LND_MACAROONPATH")

	if Config.ElementsPass == "" || Config.ElementsUser == "" {
		// disable Liquid so that peerswapd does not fail
		t += "liquidswaps=false\n"
		// enable Bitcoin swaps because both cannot be disabled
		t += "bitcoinswaps=true\n"
	} else {
		t += setPeerswapdVariable("liquidswaps", "true", "true", "")
		t += setPeerswapdVariable("elementsd.rpcuser", "", Config.ElementsUser, "ELEMENTS_USER")
		t += setPeerswapdVariable("elementsd.rpcpass", "", Config.ElementsPass, "ELEMENTS_PASS")
		t += setPeerswapdVariable("elementsd.rpchost", "http://127.0.0.1", Config.ElementsHost, "ELEMENTS_HOST")
		t += setPeerswapdVariable("elementsd.rpcport", "18884", Config.ElementsPort, "ELEMENTS_PORT")
		t += setPeerswapdVariable("bitcoinswaps", "false", strconv.FormatBool(Config.BitcoinSwaps), "")
	}

	t += setPeerswapdVariable("elementsd.rpcwallet", "peerswap", Config.ElementsWallet, "ELEMENTS_WALLET")

	logLevel := "1"
	if os.Getenv("DEBUG") == "1" {
		logLevel = "2"
	}
	t += setPeerswapdVariable("loglevel", "2", logLevel, "")

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

func setPeerswapdVariable(variableName, defaultValue, newValue, envKey string) string {
	// returns variable=value string\n
	// priority:
	// 1. newValue
	// 2. envValue
	// 3. oldValue from peerswap.conf
	// 4. defaultValue

	v := defaultValue
	if newValue != "" {
		v = newValue
	} else if envKey != "" && os.Getenv(envKey) != "" {
		v = os.Getenv(envKey)
	} else if s := GetPeerswapLNDSetting(variableName); s != "" {
		v = s
	}
	return variableName + "=" + v + "\n"
}

func GetPeerswapLNDSetting(searchVariable string) string {
	filePath := filepath.Join(Config.DataDir, "peerswap.conf")
	return getConfSetting(searchVariable, filePath)
}

func getLndConfSetting(searchVariable string) string {
	filePath := filepath.Join(Config.LightningDir, "lnd.conf")
	return getConfSetting(searchVariable, filePath)
}

func getConfSetting(searchVariable, filePath string) string {
	// Read the entire content of the file
	content, err := os.ReadFile(filePath)
	if err != nil {
		return ""
	}
	// Convert the content to a string
	fileContent := string(content)

	if index := strings.Index(fileContent, searchVariable); index > 0 {
		startIndex := index + len(searchVariable) + 1
		value := ""
		for _, char := range fileContent[startIndex:] {
			if char == '\n' || char == '\r' {
				break
			}
			value += string(char)
		}
		return value
	}
	return ""
}