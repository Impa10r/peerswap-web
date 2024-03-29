package main

import (
	"encoding/json"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

type Configuration struct {
	AllowSwapRequests    bool
	RpcHost              string
	ListenPort           string
	ColorScheme          string
	BitcoinApi           string // for bitcoin tx links
	LiquidApi            string // for liquid tx links
	NodeApi              string // for node links
	MaxHistory           uint
	DataDir              string
	ElementsUser         string
	ElementsPass         string
	BitcoinSwaps         bool
	Chain                string
	LocalMempool         string
	ElementsDir          string // what Elements see inside its docker container
	ElementsDirMapped    string // what will be mapped to PeerSwap docker
	ElementsBackupAmount uint64
	TelegramToken        string
	TelegramChatId       int64
	PeginClaimScript     string
	PeginTxId            string
	PeginAmount          int64
	LndDir               string
	BitcoinHost          string
	BitcoinUser          string
	BitcoinPass          string
	ProxyURL             string
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
		dataDir = filepath.Join(currentUser.HomeDir, ".peerswap")
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
	config.ElementsDir = filepath.Join(currentUser.HomeDir, ".elements")
	config.ElementsDirMapped = filepath.Join(currentUser.HomeDir, ".elements")
	config.LndDir = filepath.Join(currentUser.HomeDir, ".lnd")

	host := getLndConfSetting("bitcoind.rpchost")
	port := "8332"

	// environment values take priority
	if os.Getenv("ELEMENTS_FOLDER") != "" {
		config.ElementsDir = os.Getenv("ELEMENTS_FOLDER")
	}

	if os.Getenv("ELEMENTS_FOLDER_MAPPED") != "" {
		config.ElementsDirMapped = os.Getenv("ELEMENTS_FOLDER_MAPPED")
	}

	if os.Getenv("NETWORK") == "testnet" {
		config.Chain = "testnet"
		config.NodeApi = "https://mempool.space/testnet/lightning/node"
		config.BitcoinApi = "https://mempool.space/testnet"
		config.LiquidApi = "https://liquid.network/testnet"
		port = "18332"
	}

	if host == "" {
		config.BitcoinHost = getBlockIoHost()
		config.BitcoinUser = ""
		config.BitcoinPass = ""
	} else {
		config.BitcoinHost = "http://" + host + ":" + port
		config.BitcoinUser = getLndConfSetting("bitcoind.rpcuser")
		config.BitcoinPass = getLndConfSetting("bitcoind.rpcpass")
	}

	configFile := filepath.Join(dataDir, "pswebconfig.json")

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
	filename := filepath.Join(config.DataDir, "pswebconfig.json")
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
	filename := filepath.Join(config.DataDir, "peerswap.conf")
	defaultLndDir := filepath.Join(currentUser.HomeDir, "/.lnd")

	t := "# Config managed by PeerSwap Web UI\n"
	t += "# It is not recommended to modify this file directly\n\n"
	//key, default, new value, env key
	t += setPeerswapdVariable("host", "localhost:42069", config.RpcHost, "")
	t += setPeerswapdVariable("rpchost", "localhost:42070", "", "")
	t += setPeerswapdVariable("lnd.host", "localhost:10009", "", "LND_HOST")
	t += setPeerswapdVariable("lnd.tlscertpath", filepath.Join(defaultLndDir, "tls.cert"), "", "")
	t += setPeerswapdVariable("lnd.macaroonpath", filepath.Join(defaultLndDir, "data", "chain", "bitcoin", config.Chain, "admin.macaroon"), "", "LND_MACAROONPATH")

	if config.ElementsPass == "" || config.ElementsUser == "" {
		// disable Liquid so that peerswapd does not fail
		t += setPeerswapdVariable("liquidswaps", "false", "", "")
		// enable Bitcoin swaps because both cannot be disabled
		t += setPeerswapdVariable("bitcoinswaps", "false", "true", "")
	} else {
		t += setPeerswapdVariable("liquidswaps", "true", "true", "")
		t += setPeerswapdVariable("elementsd.rpcuser", "", config.ElementsUser, "ELEMENTS_USER")
		t += setPeerswapdVariable("elementsd.rpcpass", "", config.ElementsPass, "ELEMENTS_PASS")
		t += setPeerswapdVariable("elementsd.rpchost", "http://127.0.0.1", "", "ELEMENTS_HOST")
		t += setPeerswapdVariable("elementsd.rpcport", "18884", "", "ELEMENTS_PORT")
		t += setPeerswapdVariable("bitcoinswaps", "false", strconv.FormatBool(config.BitcoinSwaps), "")
	}

	t += setPeerswapdVariable("elementsd.rpcwallet", "peerswap", "", "ELEMENTS_WALLET")

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
	} else if s := getPeerswapConfSetting(variableName); s != "" {
		v = s
	}
	return variableName + "=" + v + "\n"
}

func getPeerswapConfSetting(searchVariable string) string {
	filePath := filepath.Join(config.DataDir, "peerswap.conf")
	return getConfSetting(searchVariable, filePath)
}

func getLndConfSetting(searchVariable string) string {
	filePath := filepath.Join(config.LndDir, "lnd.conf")
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

func getBlockIoHost() string {
	if os.Getenv("NETWORK") == "testnet" {
		return "https://go.getblock.io/af084a9cb73840be95696eb29b5165e0"
	} else {
		return "https://go.getblock.io/6885fe0778944e28979adc739c7105b6"
	}
}
