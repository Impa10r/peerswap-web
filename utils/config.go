package utils

import (
	"encoding/json"
	"log"
	"os"
	"os/user"
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
	ConfigFile        string
}

var Config Configuration

func LoadConfig(configFile string) {

	if configFile == "" {
		// Get the current user's information
		currentUser, err := user.Current()
		if err != nil {
			log.Fatalln(err)
		}
		// Default is /home/user/.peerswap/pswebconfig.json
		configFile = currentUser.HomeDir + "/.peerswap/pswebconfig.json"
	}

	// load defaults first
	Config.AllowSwapRequests = true
	Config.RpcHost = "localhost:42069"
	Config.ListenPort = "8088"
	Config.ColorScheme = "dark" // dark or light
	Config.NodeApi = "https://mempool.space/testnet/lightning/node"
	Config.BitcoinApi = "https://mempool.space/testnet"
	Config.LiquidApi = "https://liquid.network/testnet"
	Config.MaxHistory = 10
	Config.ConfigFile = configFile

	fileData, err := os.ReadFile(configFile)
	if err != nil {
		err = SaveConfig()
		if err != nil {
			log.Println("Error creating config file.", err)
		} else {
			log.Println("Config file created using defaults.")
		}
		return
	}

	err = json.Unmarshal(fileData, &Config)
	if err != nil {
		log.Println("Error unmarshalling config file. Using defaults.")
	}
}

func SaveConfig() error {
	jsonData, err := json.MarshalIndent(Config, "", "  ")
	if err != nil {
		return err
	}
	filename := Config.ConfigFile
	err = os.WriteFile(filename, jsonData, 0644)
	if err != nil {
		return err
	}

	return nil
}
