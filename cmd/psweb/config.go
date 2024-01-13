package main

import (
	"encoding/json"
	"log"
	"os"
	"os/user"
	"strconv"
	"strings"
)

type Configuration struct {
	AllowSwapRequests bool
	RpcHost           string
	ListenPort        string
	ColorScheme       string
	BitcoinApi        string // for bitcoin tx links
	LiquidApi         string // for liquid tx links
	NodeApi           string // for node links
	MaxHistory        uint
	DataDir           string
	ElementsUser      string
	ElementsPass      string
	BitcoinSwaps      bool
	Chain             string
	LocalMempool      string
	ElementsDir       string // what Elements see inside its docker container
	ElementsDirMapped string // what will be mapped to PeerSwap docker
}

var config Configuration

func loadConfig(dataDir string) {

	// Get the current user's information
	currentUser, err := user.Current()
	if err != nil {
		log.Fatalln(err)
	}
	if dataDir == "" {

		// Default is /home/user/.peerswap
		dataDir = currentUser.HomeDir + "/.peerswap"
	}

	// load defaults first
	config.AllowSwapRequests = true
	config.RpcHost = "localhost:42069"
	config.ColorScheme = "dark" // dark or light
	config.NodeApi = "https://amboss.space/node"
	config.BitcoinApi = "https://mempool.space"
	config.LiquidApi = "https://liquid.network"
	config.MaxHistory = 10
	config.DataDir = dataDir
	config.ElementsPass = ""
	config.BitcoinSwaps = true
	config.Chain = "mainnet"
	config.LocalMempool = ""
	config.ListenPort = "1984"
	config.ElementsDir = currentUser.HomeDir + "/.elements"
	config.ElementsDirMapped = currentUser.HomeDir + "/.elements"

	// environment values take priority
	if os.Getenv("NETWORK") == "testnet" {
		config.Chain = "testnet"
		config.NodeApi = "https://mempool.space/testnet/lightning/node"
		config.BitcoinApi = "https://mempool.space/testnet"
		config.LiquidApi = "https://liquid.network/testnet"
	}

	if os.Getenv("ELEMENTS_FOLDER") != "" {
		config.ElementsDir = os.Getenv("ELEMENTS_FOLDER")
	}

	if os.Getenv("ELEMENTS_FOLDER_MAPPED") != "" {
		config.ElementsDirMapped = os.Getenv("ELEMENTS_FOLDER_MAPPED")
	}

	configFile := dataDir + "/pswebconfig.json"

	fileData, err := os.ReadFile(configFile)
	if err != nil {
		// save defaults in a newly created file
		err = saveConfig()
		if err != nil {
			log.Println("Error creating config file.", err)
		} else {
			log.Println("Config file created using defaults.")
		}
		return
	}

	err = json.Unmarshal(fileData, &config)
	if err != nil {
		log.Println("Error unmarshalling config file. Using defaults.")
	}
}

func saveConfig() error {
	jsonData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	filename := config.DataDir + "/pswebconfig.json"
	err = os.WriteFile(filename, jsonData, 0644)
	if err != nil {
		return err
	}
	return nil
}

func savePeerSwapdConfig() {
	// Get the current user's information
	currentUser, err := user.Current()
	if err != nil {
		log.Fatalln(err)
	}
	filename := config.DataDir + "/peerswap.conf"
	defaultLndDir := currentUser.HomeDir + "/.lnd"

	//key, default, new value, env key
	t := setPeerswapdVariable("host", "localhost:42069", config.RpcHost, "")
	t += setPeerswapdVariable("rpchost", "localhost:42070", "", "")
	t += setPeerswapdVariable("lnd.host", "localhost:10009", "", "LND_HOST")
	t += setPeerswapdVariable("lnd.tlscertpath", defaultLndDir+"/tls.cert", "", "")
	t += setPeerswapdVariable("lnd.macaroonpath", defaultLndDir+"/data/chain/bitcoin/"+config.Chain+"/admin.macaroon", "", "LND_MACAROONPATH")

	if config.ElementsPass == "" || config.ElementsUser == "" {
		// remove Liquid from config so that peerswapd keeps running
		t += setPeerswapdVariable("liquidswaps", "false", "", "")
	} else {
		t += setPeerswapdVariable("elementsd.rpcuser", "", config.ElementsUser, "ELEMENTS_USER")
		t += setPeerswapdVariable("elementsd.rpcpass", "", config.ElementsPass, "ELEMENTS_PASS")
		t += setPeerswapdVariable("elementsd.rpchost", "http://127.0.0.1", "", "ELEMENTS_HOST")
		t += setPeerswapdVariable("elementsd.rpcport", "18884", "", "ELEMENTS_PORT")
		t += setPeerswapdVariable("elementsd.rpcwallet", "peerswap", "", "ELEMENTS_WALLET")
		t += setPeerswapdVariable("bitcoinswaps", "false", strconv.FormatBool(config.BitcoinSwaps), "")
		t += setPeerswapdVariable("liquidswaps", "true", "true", "")
	}

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
	} else if s := readVariableFromPeerswapdConfig(variableName); s != "" {
		v = s
	}
	return variableName + "=" + v + "\n"
}

func readVariableFromPeerswapdConfig(searchVariable string) string {
	// Read the entire content of the file
	filePath := config.DataDir + "/peerswap.conf"
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
