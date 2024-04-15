//go:build cln

package ln

import (
	"log"
	"sort"

	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/mempool"

	"github.com/elementsproject/glightning/glightning"
	"github.com/lightningnetwork/lnd/lnrpc"
)

const fileRPC = "lightning-rpc"

func GetClient() (*glightning.Lightning, func(), error) {
	lightning := glightning.NewLightning()
	err := lightning.StartUp(fileRPC, config.Config.RpcHost)
	if err != nil {
		log.Println("PS CLN Connection:", err)
		return nil, nil, err
	}

	cleanup := func() {
		//lightning.Shutdown()
	}

	return lightning, cleanup, nil
}

func SendCoins(client *glightning.Lightning, addr string, amount int64, feeRate uint64, sweepall bool, label string) (string, error) {
	minConf := uint16(1)
	res, err := client.Withdraw(addr, &glightning.Sat{
		Value:   uint64(amount),
		SendAll: sweepall,
	}, &glightning.FeeRate{
		Rate: uint(feeRate * 1000),
	}, &minConf)

	if err != nil {
		log.Println("WithdrawRequest:", err)
		return "", err
	}

	return res.TxId, nil
}

func ConfirmedWalletBalance(client *glightning.Lightning) int64 {
	var response map[string]interface{}

	err := client.Request(&glightning.ListFundsRequest{}, &response)
	if err != nil {
		log.Println("ListFunds:", err)
		return 0
	}

	// Total amount_msat
	var totalAmount int64

	// Iterate over outputs and add amount_msat to total
	outputs := response["outputs"].([]interface{})
	for _, output := range outputs {
		outputMap := output.(map[string]interface{})
		if outputMap["status"] == "confirmed" {
			amountMsat := outputMap["amount_msat"].(float64)
			totalAmount += int64(amountMsat / 1000)
		}
	}

	return totalAmount
}

func ListUnspent(client *glightning.Lightning, list *[]UTXO) error {
	res, err := client.GetInfo()
	if err != nil {
		log.Println("GetInfo:", err)
		return err
	}

	tip := res.Blockheight

	var response map[string]interface{}
	err = client.Request(&glightning.ListFundsRequest{}, &response)
	if err != nil {
		log.Println("ListFundsRequest:", err)
		return err
	}

	// Dereference the pointer to get the actual array
	a := *list

	// Iterate over outputs and append to a list
	outputs := response["outputs"].([]interface{})
	for _, output := range outputs {
		outputMap := output.(map[string]interface{})
		amountMsat := outputMap["amount_msat"].(float64)
		status := outputMap["status"].(string)
		confs := int64(0)
		if status == "confirmed" {
			blockHeight := outputMap["blockheight"].(float64)
			confs = int64(tip - uint(blockHeight))
		}

		a = append(a, UTXO{
			Address:       outputMap["address"].(string),
			AmountSat:     int64(amountMsat / 1000),
			Confirmations: confs,
		})
	}

	// sort the table on Confirmations field
	sort.Slice(a, func(i, j int) bool {
		return a[i].Confirmations < a[j].Confirmations
	})

	// Update the array through the pointer
	*list = a

	return nil
}

func GetTxConfirmations(client *glightning.Lightning, txid string) int32 {
	res, err := client.GetInfo()
	if err != nil {
		log.Println("GetInfo:", err)
		return 0
	}

	tip := int32(res.Blockheight)

	height := mempool.GetTxHeight(txid)

	if height == 0 {
		return 0
	}
	return tip - height
}

func GetAlias(nodeKey string) string {
	// not implemented, use mempool
	return ""
}

func BumpFee(TxId string, outputIndex uint32, newFeeRate uint64) error {
	// not implemented
	return nil
}

func GetTransaction(txid string) (*lnrpc.Transaction, error) {
	// not implemented
	return nil, nil
}
