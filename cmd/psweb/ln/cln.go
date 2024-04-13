//go:build clnversion

package ln

import (
	"log"

	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/mempool"

	"github.com/elementsproject/glightning/glightning"
	"github.com/lightningnetwork/lnd/lnrpc"
)

const fileRPC = "lightning-rpc"

func getClient() (*glightning.Lightning, func(), error) {
	lightning := glightning.NewLightning()
	err := lightning.StartUp(fileRPC, config.Config.RpcHost)
	if err != nil {
		log.Println("clnConnection", err)
		return nil, nil, err
	}

	cleanup := func() { lightning.Shutdown() }

	return lightning, cleanup, nil
}

func SendCoins(addr string, amount int64, feeRate uint64, sweepall bool, label string) (string, error) {
	client, cleanup, err := getClient()
	if err != nil {
		return "", err
	}
	defer cleanup()

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

func ConfirmedWalletBalance() int64 {
	client, cleanup, err := getClient()
	if err != nil {
		return 0
	}
	defer cleanup()

	var response map[string]interface{}

	err = client.Request(&glightning.ListFundsRequest{}, &response)
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

func ListUnspent(list *[]UTXO) error {
	client, cleanup, err := getClient()
	if err != nil {
		return err
	}
	defer cleanup()

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

	// Iterate over outputs and append to a
	outputs := response["outputs"].([]interface{})
	for _, output := range outputs {
		outputMap := output.(map[string]interface{})
		amountMsat := outputMap["amount_msat"].(float64)
		blockHeight := outputMap["blockheight"].(float64)
		a = append(a, UTXO{
			Address:       outputMap["address"].(string),
			AmountSat:     int64(amountMsat / 1000),
			Confirmations: int64(tip - uint(blockHeight)),
		})
	}

	// Update the array through the pointer
	*list = a

	return nil
}

func GetTxConfirmations(txid string) int32 {
	client, cleanup, err := getClient()
	if err != nil {
		return 0
	}
	defer cleanup()

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
