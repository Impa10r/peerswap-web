//go:build cln

package ln

import (
	"errors"
	"log"
	"os/exec"
	"sort"
	"strconv"

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
		Rate: uint(feeRate * 1000),
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

func BumpPeginFee(newFeeRate uint64) error {

	client, clean, err := GetClient()
	if err != nil {
		log.Println("GetClient:", err)
		return err
	}
	defer clean()

	tx, err := getTransaction(client, config.Config.PeginTxId)
	if err != nil {
		return err
	}
	// BUG: outputs show "" Satoshis, must decode raw

	decodedTx, err := bitcoin.DecodeRawTransaction(tx.RawTx)
	if err != nil {
		return err
	}

	if len(decodedTx.Vout) == 1 {
		return errors.New("peg-in transaction has no change output, not possible to CPFP")
	}

	addr := ""
	outputIndex := uint(999) // will fail if output not found
	for _, output := range decodedTx.Vout {
		if toSats(output.Value) != config.Config.PeginAmount {
			outputIndex = output.N
			addr = output.ScriptPubKey.Address // re-use address
			break
		}
	}

	utxos := []*glightning.Utxo{
		&glightning.Utxo{
			TxId:  config.Config.PeginTxId,
			Index: outputIndex,
		},
	}

	// check that the output is not reserved
	var response map[string]interface{}
	err = client.Request(&glightning.Method_GetUtxOut{
		TxId: config.Config.PeginTxId,
		Vout: uint32(outputIndex),
	}, &response)

	if err != nil {
		log.Println("Method_GetUtxOut:", err)
		return err
	}

	if response["amount"] == nil {
		// need to unreserve utxo
		vin := config.Config.PeginTxId + ":" + strconv.FormatUint(uint64(outputIndex), 10)
		cmd := "bash"
		args := []string{"-c", "psbt=$(lightning-cli utxopsbt -k satoshi=\"all\" feerate=3000perkb startweight=0 utxos='[\"" + vin + "\"]' reserve=0 reservedok=true | jq -r .psbt) && lightning-cli unreserveinputs -k psbt=\"$psbt\" reserve=1000"}

		log.Println("Unreserving utxo using bash command...")

		_, err := exec.Command(cmd, args...).Output()
		if err != nil {
			log.Println("Command:", err)
			return err
		}
	}

	minConf := uint16(0)
	res, err := client.WithdrawWithUtxos(addr, &glightning.Sat{
		Value:   uint64(0),
		SendAll: true,
	}, &glightning.FeeRate{
		Rate: uint(newFeeRate * 932), // this closer translates to sat/vB when SendAll
	}, &minConf, utxos)

	if err != nil {
		log.Println("WithdrawWithUtxos:", err)
		return err
	}

	// use mempool.space to broadcast raw tx
	mempool.SendRawTransaction(res.Tx)

	log.Println("Fee bump successful, child TxId:", res.TxId)
	return nil
}

// returns "1234sat" amount of output, "" if non-existent
func getUtxOut(client *glightning.Lightning, txId string, vout uint32) (string, error) {
	var response map[string]interface{}

	err := client.Request(&glightning.Method_GetUtxOut{
		TxId: txId,
		Vout: vout,
	}, &response)

	if err != nil {
		log.Println("Method_GetUtxOut:", err)
		return "", err
	}

	if response["amount"] == nil {
		return "", nil
	} else {
		return response["amount"].(string), nil
	}
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

func FindChildTx(client *glightning.Lightning) string {
	tx, err := getTransaction(client, config.Config.PeginTxId)
	if err != nil {
		return ""
	}

	decodedTx, err := bitcoin.DecodeRawTransaction(tx.RawTx)
	if err != nil {
		return ""
	}

	if len(decodedTx.Vout) == 1 {
		return ""
	}

	outputIndex := uint(999) // will fail if output not found
	for _, output := range decodedTx.Vout {
		if toSats(output.Value) != config.Config.PeginAmount {
			outputIndex = output.N
			break
		}
	}

	txs, err := client.ListTransactions()
	if err != nil {
		return ""
	}

	txid := ""
	for _, tx := range txs {
		for _, in := range tx.Inputs {
			// find the latest child spending our output
			if in.TxId == config.Config.PeginTxId && in.Index == outputIndex {
				txid = tx.Hash
			}
		}
	}

	return txid
}

func toSats(amount float64) int64 {
	return int64(float64(100000000) * amount)
}
