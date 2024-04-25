//go:build cln

package ln

import (
	"errors"
	"log"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"peerswap-web/cmd/psweb/bitcoin"
	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/mempool"

	"github.com/elementsproject/glightning/glightning"
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
		Rate: uint(feeRate * 932), // better translates to sat/vB
	}, &minConf)

	if err != nil {
		log.Println("Withdraw:", err)
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

func ListUnspent(client *glightning.Lightning, list *[]UTXO, minConfs int32) error {
	res, err := client.GetInfo()
	if err != nil {
		log.Println("GetInfo:", err)
		return err
	}

	tip := res.Blockheight

	var response map[string]interface{}
	err = client.Request(&glightning.ListFundsRequest{}, &response)
	if err != nil {
		log.Println("ListFunds:", err)
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
		if confs >= int64(minConfs) {
			a = append(a, UTXO{
				Address:       outputMap["address"].(string),
				AmountSat:     int64(amountMsat / 1000),
				Confirmations: confs,
			})
		}
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

func RbfPegin(newFeeRate uint64) (string, int64, error) {
	client, clean, err := GetClient()
	if err != nil {
		log.Println("GetClient:", err)
		return "", 0, err
	}
	defer clean()

	tx, err := getTransaction(client, config.Config.PeginTxId)
	if err != nil {
		return "", 0, err
	}

	decodedTx, err := bitcoin.DecodeRawTransaction(tx.RawTx)
	if err != nil {
		return "", 0, err
	}

	vins := ""
	var utxos []string

	for _, input := range decodedTx.Vin {
		vin := input.TXID + ":" + strconv.FormatUint(uint64(input.Vout), 10)
		utxos = append(utxos, vin)
		if vins != "" {
			vins += ","
		}
		vins += "\"" + vin + "\""
	}

	// unreserve utxos
	cmd := "bash"
	args := []string{"-c", "psbt=$(lightning-cli utxopsbt -k satoshi=\"all\" feerate=3000perkb startweight=0 utxos='[" + vins + "]' reserve=0 reservedok=true | jq -r .psbt) && lightning-cli unreserveinputs -k psbt=\"$psbt\" reserve=1000"}

	out, err := exec.Command(cmd, args...).Output()
	if err != nil {
		log.Println("Command:", err)
		log.Println("Output:", out)
		return "", 0, err
	}

	sendAll := len(decodedTx.Vout) == 1

	return PrepareRawTxWithUtxos(
		&utxos,
		config.Config.PeginAddress,
		config.Config.PeginAmount,
		newFeeRate,
		sendAll)
}

func getTransaction(client *glightning.Lightning, txid string) (*glightning.Transaction, error) {
	txs, err := client.ListTransactions()
	if err != nil {
		log.Println("ListTransactions", err)
		return nil, err
	}

	for _, tx := range txs {
		if tx.Hash == txid {
			return &tx, nil
		}
	}

	log.Println("getTransaction:", "transaction "+txid+" not found")
	return nil, errors.New("transaction " + txid + " not found")
}

func GetRawTransaction(client *glightning.Lightning, txid string) (string, error) {
	tx, err := getTransaction(client, txid)

	if err != nil {
		return "", err
	}

	return tx.RawTx, nil
}

// utxos: ["txid:index", ....]
// return rawTx hex string, final output amount, error
func PrepareRawTxWithUtxos(utxos *[]string, addr string, amount int64, feeRate uint64, subtractFeeFromAmount bool) (string, int64, error) {
	client, clean, err := GetClient()
	if err != nil {
		log.Println("GetClient:", err)
		return "", 0, err
	}
	defer clean()

	inputs := []*glightning.Utxo{}
	for _, i := range *utxos {
		parts := strings.Split(i, ":")

		index, err := strconv.Atoi(parts[1])
		if err != nil {
			log.Println("Invalid UTXOs:", err)
			return "", 0, err
		}

		inputs = append(inputs, &glightning.Utxo{
			TxId:  parts[0],
			Index: uint(index),
		})
	}

	minConf := uint16(1)
	res, err := client.WithdrawWithUtxos(
		addr,
		&glightning.Sat{
			Value:   uint64(amount),
			SendAll: subtractFeeFromAmount,
		},
		&glightning.FeeRate{
			Rate: uint(feeRate * 932), // better translates to sat/vB
		},
		&minConf,
		inputs)

	if err != nil {
		log.Println("WithdrawWithUtxos:", err)
		return "", 0, err
	}

	amountSent := config.Config.PeginAmount
	if subtractFeeFromAmount {
		decodedTx, err := bitcoin.DecodeRawTransaction(res.Tx)
		if err == nil && len(decodedTx.Vout) == 1 {
			amountSent = toSats(decodedTx.Vout[0].Value)
		}
	}
	return res.Tx, amountSent, nil
}
