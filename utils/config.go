package utils

import (
	"encoding/json"
	"log"
	"os"
	"os/user"
)

type Configuration struct {
	RpcHost     string
	ListenPort  string
	ColorScheme string
	MempoolApi  string
	LiquidApi   string
	ConfigFile  string
}

func LoadConfig(configFile string, conf *Configuration) {

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
	conf.RpcHost = "localhost:42069"
	conf.ListenPort = "8088"
	conf.ColorScheme = "dark"                         // dark or light
	conf.MempoolApi = "https://mempool.space/testnet" // https://mempool.space for mainnet
	conf.LiquidApi = "https://liquid.network/testnet" // https://liquid.network for mainnet
	conf.ConfigFile = configFile

	fileData, err := os.ReadFile(configFile)
	if err != nil {
		log.Println("Error reading config file. Using defaults.")
		// return defauls
		return
	}

	err = json.Unmarshal(fileData, conf)
	if err != nil {
		log.Println("Error unmarshalling config file. Using defaults.")
	}
}

func SaveConfig(conf *Configuration) error {
	jsonData, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return err
	}
	filename := conf.ConfigFile
	err = os.WriteFile(filename, jsonData, 0644)
	if err != nil {
		return err
	}

	return nil
}
