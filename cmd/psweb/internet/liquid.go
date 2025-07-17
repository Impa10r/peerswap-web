package internet

import (
	"encoding/json"
	"net/http"

	"peerswap-web/cmd/psweb/config"
)

// fetch transaction fee from liquid.network
func GetLiquidTxFee(txid string) int64 {
	testnet := ""
	if config.Config.Chain != "mainnet" {
		testnet = "/liquidtestnet"
	}
	api := "https://liquid.network" + testnet + "/api/tx/" + txid
	req, err := http.NewRequest("GET", api, nil)
	if err == nil {
		cl := GetHttpClient(true)
		if cl == nil {
			return 0
		}
		resp, err2 := cl.Do(req)
		if err2 == nil && resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()

			var tx map[string]interface{}

			err = json.NewDecoder(resp.Body).Decode(&tx)
			if err != nil {
				return 0
			}
			fee := tx["fee"].(float64)
			return int64(fee)
		}
	}
	return 0
}
