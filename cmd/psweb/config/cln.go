//go:build cln

package config

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config defaults for CLN
func loadDefaults(home, dataDir string) {
	Config.Implementation = "CLN"
	if dataDir == "" {
		Config.LightningDir = filepath.Join(home, ".lightning")
	} else {
		Config.LightningDir = dataDir
	}

	if os.Getenv("NETWORK") == "testnet" {
		Config.RpcHost = filepath.Join(Config.LightningDir, "testnet")
		Config.DataDir = filepath.Join(Config.RpcHost, "peerswap")
	} else {
		Config.RpcHost = filepath.Join(Config.LightningDir, "testnet")
		Config.DataDir = filepath.Join(Config.RpcHost, "peerswap")
	}
}

// CLN-specific load from Peerswap config
func LoadPS() {
	host := GetPeerswapCLNSetting("Bitcoin", "rpchost")
	user := GetPeerswapCLNSetting("Bitcoin", "rpcuser")
	password := GetPeerswapCLNSetting("Bitcoin", "rpcpassword")

	if host == "" && user == "" && password == "" {
		Config.BitcoinHost = GetBlockIoHost()
		Config.BitcoinUser = ""
		Config.BitcoinPass = ""
	} else {
		if host == "" {
			host = "http://localhost"
		}
		port := GetPeerswapCLNSetting("Bitcoin", "rpcport")
		if port == "" {
			port = "8332"
			if os.Getenv("NETWORK") == "testnet" {
				port = "18332"
			}
		}
		Config.BitcoinHost = host + ":" + port
		Config.BitcoinUser = user
		Config.BitcoinPass = password
	}

	wallet := GetPeerswapCLNSetting("Liquid", "rpcwallet")
	if wallet != "" {
		Config.ElementsWallet = wallet
	}

	ehost := GetPeerswapCLNSetting("Liquid", "rpchost")
	if ehost != "" {
		Config.ElementsHost = ehost
	}

	eport := GetPeerswapCLNSetting("Liquid", "rpcport")
	if eport != "" {
		Config.ElementsPort = eport
	}

	// on first start without config there will be no elements user and password
	if Config.ElementsPass == "" || Config.ElementsUser == "" {
		// check in peerswap.conf
		Config.ElementsPass = GetPeerswapCLNSetting("Liquid", "rpcpassword")
		Config.ElementsUser = GetPeerswapCLNSetting("Liquid", "rpcuser")

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

	if Config.ElementsPass == "" || Config.ElementsUser == "" {
		// disable Liquid so that peerswapd does not fail
		t += "[Liquid]\n"
		t += "liquidswaps=false\n\n"
		// enable Bitcoin swaps because both cannot be disabled
		t += "[Bitcoin]\n"
		t += "bitcoinswaps=true\n"
	} else {
		t += "[Liquid]\n"
		t += setPeerswapVariable("Liquid", "liquidswaps", "true", "true", "", false)
		t += setPeerswapVariable("Liquid", "rpcuser", "", Config.ElementsUser, "ELEMENTS_USER", true)
		t += setPeerswapVariable("Liquid", "rpcpassword", "", Config.ElementsPass, "ELEMENTS_PASS", true)
		t += setPeerswapVariable("Liquid", "rpchost", "http://127.0.0.1", Config.ElementsHost, "ELEMENTS_HOST", true)
		t += setPeerswapVariable("Liquid", "rpcport", "18884", Config.ElementsPort, "ELEMENTS_PORT", false)
		t += setPeerswapVariable("Liquid", "rpcwallet", "peerswap", Config.ElementsWallet, "ELEMENTS_WALLET", true)

		rpcpasswordfile := GetPeerswapCLNSetting("Liquid", "rpcpasswordfile")
		if rpcpasswordfile != "" {
			t += setPeerswapVariable("Liquid", "rpcpasswordfile", "", "", "", true)
		}

		t += "\n[Bitcoin]\n"
		t += setPeerswapVariable("Bitcoin", "bitcoinswaps", "false", strconv.FormatBool(Config.BitcoinSwaps), "", false)
	}

	t += setPeerswapVariable("Bitcoin", "rpcuser", "", "", "", true)
	t += setPeerswapVariable("Bitcoin", "rpcpassword", "", "", "", true)
	rpchost := GetPeerswapCLNSetting("Bitcoin", "rpchost")
	if rpchost != "" {
		t += setPeerswapVariable("Bitcoin", "rpchost", "", "", "", true)
	}
	cookiefilepath := GetPeerswapCLNSetting("Bitcoin", "cookiefilepath")
	if cookiefilepath != "" {
		t += setPeerswapVariable("Bitcoin", "cookiefilepath", "", "", "", true)
	}

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

func setPeerswapVariable(section, variableName, defaultValue, newValue, envKey string, isString bool) string {
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
	} else if s := GetPeerswapCLNSetting(section, variableName); s != "" {
		v = s
	}

	if isString {
		v = "\"" + v + "\""
	}

	return variableName + "=" + v + "\n"
}

func GetPeerswapCLNSetting(section, searchVariable string) string {
	filePath := filepath.Join(Config.DataDir, "peerswap.conf")

	// Read the entire content of the file
	content, err := os.ReadFile(filePath)
	if err != nil {
		return ""
	}
	// Convert the content to a string
	fileContent := string(content)
	value := ""

	// Search section
	if sectionIndex := strings.Index(fileContent, "["+section+"]"); sectionIndex > 0 {
		// Search variable
		if index := strings.Index(fileContent[sectionIndex:], searchVariable); index > 0 {
			startIndex := sectionIndex + index + len(searchVariable) + 1
			for _, char := range fileContent[startIndex:] {
				if char == '\n' || char == '\r' {
					break
				}
				if char != '"' {
					value += string(char)
				}
			}
		}
	}
	return value
}
