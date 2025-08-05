package config

import (
	"encoding/json"
	"log"
	"os"
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
	PeginFeeRate            float64
	PeginClaimJoin          bool
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
	AutoSwapPremiumLimit    int64
	SecureConnection        bool
	ServerIPs               string
	SecurePort              string
	Password                string
}

var Config Configuration

func Load(dataDir string, network string) {

	// env gets priority
	if os.Getenv("NETWORK") != "" {
		network = os.Getenv("NETWORK")
	}

	// Get the current user's information
	currentUser := os.Getenv("USER")

	// load defaults first
	Config.AllowSwapRequests = true
	Config.ColorScheme = "dark" // dark or light
	Config.MaxHistory = 20
	Config.ElementsPass = ""
	Config.BitcoinSwaps = true
	Config.LocalMempool = ""
	Config.ListenPort = "1984"
	Config.ElementsDir = filepath.Join("home", currentUser, ".elements")
	Config.ElementsDirMapped = filepath.Join("home", currentUser, ".elements")
	Config.ElementsWallet = "peerswap"
	Config.ElementsHost = "http://127.0.0.1"
	Config.ElementsPort = "18884"

	Config.Chain = network
	Config.NodeApi = "https://amboss.space/node"
	Config.BitcoinApi = "https://mempool.space"
	Config.LiquidApi = "https://liquid.network"
	Config.AutoSwapThresholdAmount = 2_000_000
	Config.AutoSwapMaxAmount = 10_000_000
	Config.AutoSwapThresholdPPM = 300
	Config.AutoSwapTargetPct = 70
	Config.SecureConnection = false
	Config.SecurePort = "1985"

	if network != "mainnet" && network != "bitcoin" {
		Config.NodeApi = "https://mempool.space/" + network + "/lightning/node"
		Config.BitcoinApi = "https://mempool.space/" + network
		Config.LiquidApi = "https://liquid.network/testnet"
		Config.ElementsPort = "7039"
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
	loadDefaults(filepath.Join("home", currentUser), dataDir, network)

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
	} else {
		err = json.Unmarshal(fileData, &Config)
		if err != nil {
			log.Println("Error unmarshalling config file. Using defaults.")
		}
	}

	// on the first start without pswebconfig.json there will be no elements user and password
	if Config.ElementsPass == "" || Config.ElementsUser == "" {
		// check in peerswap.conf
		getElementsCredentials()

		// check if they were passed as env
		if Config.ElementsUser == "" && os.Getenv("ELEMENTS_USER") != "" {
			Config.ElementsUser = os.Getenv("ELEMENTS_USER")
		}
		if Config.ElementsPass == "" && os.Getenv("ELEMENTS_PASS") != "" {
			Config.ElementsPass = os.Getenv("ELEMENTS_PASS")
		}
		// save changes
		Save()
	}

	// if ElementPass is still empty, this will create temporary peerswap.conf with Liquid disabled
	SavePS()
}

// saves PS Web config to pswebconfig.json
func Save() error {
	jsonData, err := json.MarshalIndent(Config, "", "  ")
	if err != nil {
		log.Println("Error saving config file:", err)
		return err
	}
	filename := filepath.Join(Config.DataDir, "pswebconfig.json")
	err = os.WriteFile(filename, jsonData, 0644)
	if err != nil {
		log.Println("Error saving config file:", err)
		return err
	}
	return nil
}

// fallback for Bitcoin Core API if local is unreachable
func GetBlockIoHost() string {
	if Config.Chain == "mainnet" {
		return "https://go.getblock.io/6f4b1867b4324698936d8d18ea99a245"
	} else {
		return "https://go.getblock.io/6bf5fea3b5344c43a061fd295f613be4"
	}
}

// returns hostname of the machine or host if passed via Env
func GetHostname() string {
	// Get the hostname of the machine
	hostname, _ := os.Hostname()

	// Env takes priority
	if os.Getenv("HOSTNAME") != "" {
		hostname = os.Getenv("HOSTNAME")
	}

	return hostname
}
