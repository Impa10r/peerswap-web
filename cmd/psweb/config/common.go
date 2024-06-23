package config

import (
	"encoding/json"
	"log"
	"os"
	"os/user"
	"path/filepath"
)

type Configuration struct {
	AllowSwapRequests       bool
	RpcHost                 string
	ListenPort              string
	ColorScheme             string
	BitcoinApi              string // for bitcoin tx links
	LiquidApi               string // for liquid tx links
	NodeApi                 string // for node links
	MaxHistory              uint
	DataDir                 string
	ElementsUser            string
	ElementsPass            string
	BitcoinSwaps            bool
	Chain                   string
	LocalMempool            string
	ElementsDir             string // what Elements see inside its docker container
	ElementsDirMapped       string // what will be mapped to PeerSwap docker
	ElementsBackupAmount    uint64
	ElementsHost            string
	ElementsPort            string
	ElementsWallet          string
	TelegramToken           string
	TelegramChatId          int64
	PeginClaimScript        string
	PeginTxId               string
	PeginReplacedTxId       string
	PeginAddress            string
	PeginAmount             int64
	PeginFeeRate            uint32
	LightningDir            string
	BitcoinHost             string
	BitcoinUser             string
	BitcoinPass             string
	ProxyURL                string
	AutoSwapEnabled         bool
	AutoSwapThresholdAmount uint64
	AutoSwapMaxAmount       uint64
	AutoSwapThresholdPPM    uint64
	AutoSwapTargetPct       uint64
	SecureConnection        bool
	ServerIPs               string
	SecurePort              string
	Password                string
}

var Config Configuration

func Load(dataDir string) {

	// Get the current user's information
	currentUser, err := user.Current()
	if err != nil {
		log.Fatalln(err)
	}

	// load defaults first
	Config.AllowSwapRequests = true
	Config.ColorScheme = "dark" // dark or light
	Config.MaxHistory = 20
	Config.ElementsPass = ""
	Config.BitcoinSwaps = true
	Config.LocalMempool = ""
	Config.ListenPort = "1984"
	Config.ElementsDir = filepath.Join(currentUser.HomeDir, ".elements")
	Config.ElementsDirMapped = filepath.Join(currentUser.HomeDir, ".elements")
	Config.ElementsWallet = "peerswap"
	Config.ElementsHost = "http://127.0.0.1"
	Config.ElementsPort = "18884"

	Config.Chain = "mainnet"
	Config.NodeApi = "https://amboss.space/node"
	Config.BitcoinApi = "https://mempool.space"
	Config.LiquidApi = "https://liquid.network"
	Config.AutoSwapThresholdAmount = 2000000
	Config.AutoSwapThresholdPPM = 300
	Config.AutoSwapTargetPct = 50
	Config.SecureConnection = false
	Config.SecurePort = "1985"

	if os.Getenv("NETWORK") == "testnet" {
		Config.Chain = "testnet"
		Config.NodeApi = "https://mempool.space/testnet/lightning/node"
		Config.BitcoinApi = "https://mempool.space/testnet"
		Config.LiquidApi = "https://liquid.network/testnet"
	}

	// environment values take priority
	if os.Getenv("ELEMENTS_FOLDER") != "" {
		Config.ElementsDir = os.Getenv("ELEMENTS_FOLDER")
	}

	if os.Getenv("ELEMENTS_FOLDER_MAPPED") != "" {
		Config.ElementsDirMapped = os.Getenv("ELEMENTS_FOLDER_MAPPED")
	}

	if os.Getenv("ELEMENTS_PORT") != "" {
		Config.ElementsPort = os.Getenv("ELEMENTS_PORT")
	}

	if os.Getenv("ELEMENTS_HOST") != "" {
		Config.ElementsHost = os.Getenv("ELEMENTS_HOST")
	}

	// different defaults for LND and CLN
	loadDefaults(currentUser.HomeDir, dataDir)

	// load config from peerswap.conf
	LoadPS()

	configFile := filepath.Join(Config.DataDir, "pswebconfig.json")

	fileData, err := os.ReadFile(configFile)
	if err != nil {
		// save defaults in a newly created file
		err = Save()
		if err != nil {
			log.Println("Error creating config file.", err)
		} else {
			log.Println("Config file created in", Config.DataDir)
		}
		return
	}

	err = json.Unmarshal(fileData, &Config)
	if err != nil {
		log.Println("Error unmarshalling config file. Using defaults.")
	}

}

// saves PS Web config to pswebconfig.json
func Save() error {
	jsonData, err := json.MarshalIndent(Config, "", "  ")
	if err != nil {
		return err
	}
	filename := filepath.Join(Config.DataDir, "pswebconfig.json")
	err = os.WriteFile(filename, jsonData, 0644)
	if err != nil {
		return err
	}
	return nil
}

// fallback for Bitcoin Core API if local is unreachable
func GetBlockIoHost() string {
	if os.Getenv("NETWORK") == "testnet" {
		return "https://go.getblock.io/af084a9cb73840be95696eb29b5165e0"
	} else {
		return "https://go.getblock.io/6885fe0778944e28979adc739c7105b6"
	}
}

// returns hostname of the machine or host if passed via Env
func GetHostname() string {
	// Get the hostname of the machine
	hostname, _ := os.Hostname()

	if os.Getenv("HOSTNAME") != "" {
		hostname = os.Getenv("HOSTNAME")
	}

	return hostname
}
