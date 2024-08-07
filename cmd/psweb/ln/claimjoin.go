//go:build !cln

package ln

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strconv"
	"time"

	mathRand "math/rand"

	"github.com/btcsuite/btcd/btcec/v2"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"

	"peerswap-web/cmd/psweb/bitcoin"
	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/db"
	"peerswap-web/cmd/psweb/internet"
	"peerswap-web/cmd/psweb/liquid"
	"peerswap-web/cmd/psweb/ps"
)

// maximum number of participants in ClaimJoin
const (
	maxParties  = 2
	PeginBlocks = 10 //2
)

var (
	// encryption private key
	myPrivateKey *btcec.PrivateKey
	// maps learned public keys to node Id
	keyToNodeId = make(map[string]string)
	// public key of the sender of pegin_started broadcast
	PeginHandler string
	// when currently pending pegin can be claimed
	ClaimBlockHeight uint32
	// human readable status of the claimjoin
	ClaimStatus = "No ClaimJoin peg-in is pending"
	// none, initiator or joiner
	MyRole = "none"
	// array of initiator + joiners, for initiator only
	claimParties []*ClaimParty
	// PSET to be blinded and signed by all parties
	claimPSET string
)

type Coordination struct {
	// possible actions: add, confirm_add, refuse_add, process, process2, remove
	Action string
	// new joiner details
	Joiner ClaimParty
	// ETA of currently pending pegin claim
	ClaimBlockHeight uint32
	// human readable status of the claimjoin
	Status string
	// partially signed elements transaction
	PSET string
}

type ClaimParty struct {
	// pegin txid
	TxId string
	// pegin vout
	Vout uint
	// pegin claim script
	ClaimScript string
	// Liquid address to receive funds
	Address string
	// when can be claimed
	ClaimBlockHeight uint32
	// to be filled locally by initiator
	RawTx       string
	TxoutProof  string
	Amount      uint64
	FeeShare    uint64
	PubKey      string
	LastMessage *Coordination
	SentTime    time.Time
}

// runs after restart, to continue if pegin is ongoing
func loadClaimJoinDB() {
	db.Load("ClaimJoin", "PeginHandler", &PeginHandler)
	db.Load("ClaimJoin", "ClaimBlockHeight", &ClaimBlockHeight)
	db.Load("ClaimJoin", "ClaimStatus", &ClaimStatus)

	db.Load("ClaimJoin", "MyRole", &MyRole)
	db.Load("ClaimJoin", "keyToNodeId", &keyToNodeId)
	db.Load("ClaimJoin", "claimParties", &claimParties)

	if MyRole == "initiator" {
		db.Load("ClaimJoin", "claimPSET", &claimPSET)
	}

	var serializedKey []byte
	db.Load("ClaimJoin", "serializedPrivateKey", &serializedKey)
	myPrivateKey, _ = btcec.PrivKeyFromBytes(serializedKey)
}

// runs every minute
func OnTimer() {
	if !config.Config.PeginClaimJoin || MyRole != "initiator" || len(claimParties) < 2 {
		return
	}

	cl, clean, er := GetClient()
	if er != nil {
		return
	}
	defer clean()

	if GetBlockHeight(cl) < ClaimBlockHeight {
		// not yet matured
		return
	}

	startingFee := 36 * len(claimParties)

	if claimPSET == "" {
		var err error
		claimPSET, err = createClaimPSET(startingFee)
		if err != nil {
			return
		}
		db.Save("ClaimJoin", "claimPSET", &claimPSET)
	}

	decoded, err := liquid.DecodePSET(claimPSET)
	if err != nil {
		return
	}

	analyzed, err := liquid.AnalyzePSET(claimPSET)
	if err != nil {
		return
	}

	for i, output := range analyzed.Outputs {
		if output.Blind && output.Status == "unblinded" {
			blinder := decoded.Outputs[i].BlinderIndex
			ClaimStatus = "Blinding " + strconv.Itoa(i+1) + "/" + strconv.Itoa(len(claimParties))

			log.Println(ClaimStatus)

			if blinder == 0 {
				// my output
				claimPSET, _, err = liquid.ProcessPSET(claimPSET, config.Config.ElementsWallet)
				if err != nil {
					log.Println("Unable to blind output, cancelling ClaimJoin:", err)
					EndClaimJoin("", "Coordination failure")
					return
				}
			} else {
				action := "process"
				if i == len(claimParties)-1 {
					// the final blinder can blind and sign at once
					action = "process2"
					ClaimStatus = "Signing " + strconv.Itoa(len(claimParties)) + "/" + strconv.Itoa(len(claimParties))
					log.Println(ClaimStatus)
				}

				if !SendCoordination(claimParties[blinder].PubKey, &Coordination{
					Action:           action,
					PSET:             claimPSET,
					Status:           ClaimStatus,
					ClaimBlockHeight: ClaimBlockHeight,
				}) {
					log.Println("Unable to send blind coordination, cancelling ClaimJoin")
					EndClaimJoin("", "Coordination failure")
				}
				return
			}
		}
	}

	// Iterate through inputs in reverse order to sign
	for i := len(claimParties) - 1; i >= 0; i-- {
		input := decoded.Inputs[i]
		if len(input.FinalScriptWitness) == 0 {
			ClaimStatus = "Signing " + strconv.Itoa(len(claimParties)-i) + "/" + strconv.Itoa(len(claimParties))

			log.Println(ClaimStatus)

			if i == 0 {
				// my input, last to sign
				claimPSET, _, err = liquid.ProcessPSET(claimPSET, config.Config.ElementsWallet)
				if err != nil {
					log.Println("Unable to sign input, cancelling ClaimJoin:", err)
					EndClaimJoin("", "Initiator signing failure")
					return
				}
			} else {
				if !SendCoordination(claimParties[i].PubKey, &Coordination{
					Action:           "process",
					PSET:             claimPSET,
					Status:           ClaimStatus,
					ClaimBlockHeight: ClaimBlockHeight,
				}) {
					log.Println("Unable to send blind coordination, cancelling ClaimJoin")
					EndClaimJoin("", "Coordination failure")
				}
				return
			}
		}
	}

	if analyzed.Next == "extractor" {
		// finalize and check fee
		rawHex, done, err := liquid.FinalizePSET(claimPSET)
		if err != nil || !done {
			log.Println("Unable to finalize PSET, cancelling ClaimJoin:", err)
			EndClaimJoin("", "Cannot finalize PSET")
			return
		}

		decodedTx, err := liquid.DecodeRawTransaction(rawHex)
		if err != nil {
			log.Println("Cancelling ClaimJoin:", err)
			EndClaimJoin("", "Final TX decode failure")
			return
		}

		exactFee := (decodedTx.DiscountVsize / 10) + 1
		if decodedTx.DiscountVsize%10 == 0 {
			exactFee = (decodedTx.DiscountVsize / 10)
		}

		var feeValue int
		found := false

		// Iterate over the map
		for _, value := range decodedTx.Fee {
			feeValue = int(value * 100_000_000)
			found = true
			break
		}

		if !found {
			log.Println("Decoded transaction omits fee, cancelling ClaimJoin")
			EndClaimJoin("", "Final TX fee failure")
			return
		}

		if feeValue != exactFee {

			log.Printf("Paid fee: %d, required fee: %d, starting over", feeValue, exactFee)

			// start over with the exact fee
			claimPSET, err = createClaimPSET(exactFee)
			if err != nil {
				return
			}
			ClaimStatus = "Redo to improve fee"

			db.Save("ClaimJoin", "claimPSET", &claimPSET)
			db.Save("ClaimJoin", "ClaimStatus", &ClaimStatus)
		} else {
			// send raw transaction
			txId, err := liquid.SendRawTransaction(rawHex)
			if err != nil {
				if err.Error() == "-27: Transaction already in block chain" {
					txId = decodedTx.Txid
				} else {
					EndClaimJoin("", "Final TX send failure")
					return
				}
			}
			EndClaimJoin(txId, "done")
		}
	}
}

// Called when received a broadcast custom message
// Forward the message to all direct peers, unless the source key is known already
// (it means the message came back to you from a downstream peer)
func Broadcast(fromNodeId string, message *Message) error {

	if message.Asset == "pegin_started" && keyToNodeId[message.Sender] != "" || message.Asset == "pegin_ended" && PeginHandler == "" {
		// has been previously received from upstream
		return nil
	}

	client, cleanup, err := ps.GetClient(config.Config.RpcHost)
	if err != nil {
		return err
	}
	defer cleanup()

	res, err := ps.ListPeers(client)
	if err != nil {
		return err
	}

	cl, clean, er := GetClient()
	if er != nil {
		return err
	}
	defer clean()

	for _, peer := range res.GetPeers() {
		// don't send back to where it came from
		if peer.NodeId != fromNodeId {
			SendCustomMessage(cl, peer.NodeId, message)
		}
	}

	if myNodeId == "" {
		// populates myNodeId
		if getLndVersion() == 0 {
			return fmt.Errorf("Broadcast: Cannot get myNodeId")
		}
	}

	if fromNodeId == myNodeId {
		// do nothing more after sending the original broadcast
		return nil
	}

	switch message.Asset {
	case "pegin_started":
		if MyRole == "initiator" {
			// two simultaneous initiators conflict, agree if I have no joiners with 50% chance
			if len(claimParties) < 2 && mathRand.Intn(2) > 0 {
				MyRole = "none"
				db.Save("ClaimJoin", "MyRole", MyRole)
				log.Println("Initiator collision, switching to none")
			} else {
				log.Println("Initiator collision, staying as initiator")
				break
			}
		}
		// where to forward claimjoin request
		PeginHandler = message.Sender
		// store for relaying further encrypted messages
		keyToNodeId[message.Sender] = fromNodeId
		// Time limit to apply is communicated via Amount
		ClaimBlockHeight = uint32(message.Amount)
		ClaimStatus = "Received invitation to ClaimJoin"
		log.Println(ClaimStatus)
	case "pegin_ended":
		// only trust the message from the original handler
		if PeginHandler == message.Sender {
			txId := message.Payload
			if txId != "" {
				ClaimStatus = "ClaimJoin pegin successful! Liquid TxId: " + txId
			} else {
				ClaimStatus = "Invitations to ClaimJoin revoked"
			}
			log.Println(ClaimStatus)
			resetClaimJoin()
			return nil
		}
	}

	// persist to db
	db.Save("ClaimJoin", "MyRole", MyRole)
	db.Save("ClaimJoin", "keyToNodeId", keyToNodeId)
	db.Save("ClaimJoin", "PeginHandler", PeginHandler)
	db.Save("ClaimJoin", "ClaimBlockHeight", ClaimBlockHeight)
	db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)

	return nil
}

// Encrypt and send message to an anonymous peer identified by base64 public key
// New keys are generated at the start of each ClaimJoin session
// Peers track sources of encrypted messages to forward back the replies
func SendCoordination(destinationPubKey string, message *Coordination) bool {

	payload, err := json.Marshal(message)
	if err != nil {
		return false
	}

	destinationNodeId := keyToNodeId[destinationPubKey]
	if destinationNodeId == "" {
		log.Println("Cannot send coordination, destination PubKey has no matching NodeId")
		return false
	}

	// Encrypt the message using the base64 receiver's public key
	ciphertext, err := eciesEncrypt(destinationPubKey, payload)
	if err != nil {
		log.Println("Error encrypting message:", err)
		return false
	}

	cl, clean, er := GetClient()
	if er != nil {
		return false
	}
	defer clean()

	return SendCustomMessage(cl, destinationNodeId, &Message{
		Version:     MessageVersion,
		Memo:        "process",
		Sender:      myPublicKey(),
		Destination: destinationPubKey,
		Payload:     ciphertext,
	}) == nil
}

// Either forward to final destination or decrypt and process
func Process(message *Message, senderNodeId string) {

	if !config.Config.PeginClaimJoin {
		// spam
		return
	}

	cl, clean, er := GetClient()
	if er != nil {
		return
	}
	defer clean()

	if keyToNodeId[message.Sender] != senderNodeId {
		// save source key map
		keyToNodeId[message.Sender] = senderNodeId
		// persist to db
		db.Save("ClaimJoin", "keyToNodeId", keyToNodeId)
	}

	// log.Println("My pubKey:", myPublicKey())

	if message.Destination == myPublicKey() {
		// Convert to []byte
		payload, err := base64.StdEncoding.DecodeString(message.Payload)
		if err != nil {
			log.Printf("Error decoding payload: %s", err)
			return
		}

		// Decrypt the message using my private key
		plaintext, err := eciesDecrypt(myPrivateKey, payload)
		if err != nil {
			log.Printf("Error decrypting payload: %s", err)
			return
		}

		// recover the struct
		var msg Coordination
		err = json.Unmarshal(plaintext, &msg)
		if err != nil {
			log.Printf("Received an incorrectly formed Coordination: %s", err)
			return
		}

		claimPSET = msg.PSET

		switch msg.Action {
		case "add":
			if MyRole != "initiator" {
				log.Printf("Cannot add a joiner, not a claim initiator")
				return
			}

			if ok, status := addClaimParty(&msg.Joiner); ok {
				ClaimBlockHeight = max(ClaimBlockHeight, msg.ClaimBlockHeight)

				if SendCoordination(msg.Joiner.PubKey, &Coordination{
					Action:           "confirm_add",
					ClaimBlockHeight: ClaimBlockHeight,
					Status:           status,
				}) {
					ClaimStatus = "Added new joiner, total participants: " + strconv.Itoa(len(claimParties))
					db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)
					log.Println(ClaimStatus)
				}
			} else {
				if SendCoordination(msg.Joiner.PubKey, &Coordination{
					Action: "refuse_add",
					Status: status,
				}) {
					log.Println("Refused new joiner: ", status)
				}
			}

		case "remove":
			if MyRole != "initiator" {
				log.Printf("Cannot remove a joiner, not a claim initiator")
				return
			}

			if removeClaimParty(msg.Joiner.PubKey) {
				ClaimStatus = "Removed a joiner, total participants: " + strconv.Itoa(len(claimParties))
				// persist to db
				db.Save("ClaimJoin", "ClaimBlockHeight", ClaimBlockHeight)
				log.Println(ClaimStatus)
				// erase PSET to start over
				claimPSET = ""
				// persist to db
				db.Save("ClaimJoin", "claimPSET", claimPSET)
			} else {
				log.Println("Cannot remove joiner, not in the list")
			}

		case "confirm_add":
			ClaimBlockHeight = msg.ClaimBlockHeight
			MyRole = "joiner"
			ClaimStatus = msg.Status
			log.Println(ClaimStatus)

			// persist to db
			db.Save("ClaimJoin", "MyRole", MyRole)
			db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)

		case "refuse_add":
			log.Println(msg.Status)
			// forget pegin handler, so that cannot initiate new ClaimJoin
			PeginHandler = ""
			ClaimStatus = msg.Status
			MyRole = "none"

			// persist to db
			db.Save("ClaimJoin", "MyRole", MyRole)
			db.Save("ClaimJoin", "PeginHandler", PeginHandler)
			db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)

		case "process2": // process twice to blind and sign
			if MyRole != "joiner" {
				log.Println("received process2 while not being a joiner")
				return
			}

			// process my output
			claimPSET, _, err = liquid.ProcessPSET(claimPSET, config.Config.ElementsWallet)
			if err != nil {
				log.Println("Unable to process PSET:", err)
				return
			}
			fallthrough // continue to second pass

		case "process": // blind or sign
			if !verifyPSET() {
				log.Println("PSET verification failure!")
				if MyRole == "initiator" {
					// kick the joiner who returned broken PSET
					if ok := removeClaimParty(message.Sender); ok {
						ClaimStatus = "Joiner " + message.Sender + " kicked, total participants: " + strconv.Itoa(len(claimParties))
						// persist to db
						db.Save("ClaimJoin", "ClaimBlockHeight", ClaimBlockHeight)
						log.Println(ClaimStatus)
						// erase PSET to start over
						claimPSET = ""
						// persist to db
						db.Save("ClaimJoin", "claimPSET", claimPSET)

						SendCoordination(message.Sender, &Coordination{
							Action: "refuse_add",
							Status: "Kicked for broken PSET return",
						})
					} else {
						EndClaimJoin("", "Coordination failure")
					}
					return
				} else {
					// remove yourself from ClaimJoin
					if SendCoordination(PeginHandler, &Coordination{
						Action: "remove",
						Joiner: *claimParties[0],
					}) {
						ClaimBlockHeight = 0
						// forget pegin handler, so that cannot initiate new ClaimJoin
						PeginHandler = ""
						ClaimStatus = "Kicked from ClaimJoin"
						log.Println(ClaimStatus)

						db.Save("ClaimJoin", "ClaimStatus", &ClaimStatus)
						db.Save("ClaimJoin", "PeginHandler", &PeginHandler)

						// disable ClaimJoin
						config.Config.PeginClaimJoin = false
						config.Save()
					}
				}
				return
			}

			if MyRole == "initiator" {
				// Save received claimPSET, execute OnTimer
				db.Save("ClaimJoin", "claimPSET", &claimPSET)
				OnTimer()
				return
			}

			if MyRole != "joiner" {
				log.Println("received 'process' unexpected")
				return
			}

			// process my output
			claimPSET, _, err = liquid.ProcessPSET(claimPSET, config.Config.ElementsWallet)
			if err != nil {
				log.Println("Unable to process PSET:", err)
				return
			}

			ClaimBlockHeight = msg.ClaimBlockHeight
			ClaimStatus = msg.Status

			log.Println(ClaimStatus)

			db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)
			db.Save("ClaimJoin", "ClaimBlockHeight", ClaimBlockHeight)

			// return PSET to Handler
			if !SendCoordination(PeginHandler, &Coordination{
				Action: "process",
				PSET:   claimPSET,
			}) {
				log.Println("Unable to send blind coordination, cancelling ClaimJoin")
				EndClaimJoin("", "Coordination failure")
			}
		}
		return
	}

	// message not to me, relay further
	destinationNodeId := keyToNodeId[message.Destination]
	if destinationNodeId == "" {
		log.Println("Cannot relay: destination PubKey " + message.Destination + " has no matching NodeId")
		return
	}

	err := SendCustomMessage(cl, destinationNodeId, message)
	if err != nil {
		log.Println("Cannot relay:", err)
	}
}

// called for claim join initiator after his pegin funding tx confirms
func InitiateClaimJoin(claimBlockHeight uint32) bool {
	if myNodeId == "" {
		// populates myNodeId
		if getLndVersion() == 0 {
			return false
		}
	}

	myPrivateKey = generatePrivateKey()
	if myPrivateKey != nil {
		// persist to db
		savePrivateKey()
	} else {
		return false
	}

	if Broadcast(myNodeId, &Message{
		Version: MessageVersion,
		Memo:    "broadcast",
		Asset:   "pegin_started",
		Amount:  uint64(claimBlockHeight),
		Sender:  myPublicKey(),
	}) == nil {
		ClaimBlockHeight = claimBlockHeight
		party := createClaimParty(claimBlockHeight)
		if party != nil {
			// initiate array of claim parties
			claimParties = nil
			claimParties = append(claimParties, party)
			ClaimStatus = "Invites sent, awaiting joiners"
			// persist to db
			db.Save("ClaimJoin", "ClaimBlockHeight", ClaimBlockHeight)
			db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)
			db.Save("ClaimJoin", "claimParties", claimParties)
			return true
		}
	}
	return false
}

// called by claim join initiator after posting claim tx or on error
func EndClaimJoin(txId string, status string) bool {
	if myNodeId == "" {
		// populates myNodeId
		if getLndVersion() == 0 {
			return false
		}
	}

	if Broadcast(myNodeId, &Message{
		Version:     MessageVersion,
		Memo:        "broadcast",
		Asset:       "pegin_ended",
		Amount:      uint64(0),
		Sender:      myPublicKey(),
		Payload:     txId,
		Destination: status,
	}) == nil {
		if txId != "" {
			log.Println("ClaimJoin pegin success! Liquid TxId:", txId)
			// signal to telegram bot
			config.Config.PeginTxId = txId
			config.Config.PeginClaimScript = "done"
		} else {
			log.Println("ClaimJoin failed")
			// signal to telegram bot
			config.Config.PeginClaimScript = "failed"
		}
		resetClaimJoin()
		return true
	}
	return false
}

func resetClaimJoin() {
	// eraze all traces
	ClaimBlockHeight = 0
	claimParties = nil
	MyRole = "none"
	PeginHandler = ""
	ClaimStatus = "No ClaimJoin peg-in is pending"
	keyToNodeId = make(map[string]string)

	// persist to db
	db.Save("ClaimJoin", "claimParties", claimParties)
	db.Save("ClaimJoin", "PeginHandler", PeginHandler)
	db.Save("ClaimJoin", "ClaimBlockHeight", ClaimBlockHeight)
	db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)
	db.Save("ClaimJoin", "MyRole", MyRole)
	db.Save("ClaimJoin", "keyToNodeId", keyToNodeId)
}

// called for ClaimJoin joiner candidate after his pegin funding tx confirms
func JoinClaimJoin(claimBlockHeight uint32) bool {
	if myNodeId == "" {
		// populates myNodeId
		if getLndVersion() == 0 {
			return false
		}
	}

	myPrivateKey = generatePrivateKey()
	if myPrivateKey != nil {
		// persist to db
		savePrivateKey()
	} else {
		return false
	}

	party := createClaimParty(claimBlockHeight)
	if party != nil {
		if SendCoordination(PeginHandler, &Coordination{
			Action:           "add",
			Joiner:           *party,
			ClaimBlockHeight: claimBlockHeight,
		}) {
			// initiate array of claim parties for single entry
			claimParties = nil
			claimParties = append(claimParties, party)
			ClaimStatus = "Responded to invitation, awaiting confirmation"
			// persist to db
			db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)
			db.Save("ClaimJoin", "claimParties", claimParties)
			ClaimBlockHeight = claimBlockHeight
			return true
		}
	}
	return false
}

func createClaimParty(claimBlockHeight uint32) *ClaimParty {
	party := new(ClaimParty)
	party.TxId = config.Config.PeginTxId
	party.ClaimScript = config.Config.PeginClaimScript
	party.ClaimBlockHeight = claimBlockHeight
	party.Amount = uint64(config.Config.PeginAmount)

	var err error
	party.RawTx, err = bitcoin.GetRawTransaction(config.Config.PeginTxId, nil)
	if err != nil {
		log.Println("Cannot create ClaimParty: GetRawTransaction:", err)
		return nil
	}

	party.Vout, err = bitcoin.FindVout(party.RawTx, uint64(config.Config.PeginAmount))
	if err != nil {
		log.Println("Cannot create ClaimParty: FindVout:", err)
		return nil
	}

	party.TxoutProof, err = bitcoin.GetTxOutProof(config.Config.PeginTxId)
	if err != nil {
		log.Println("Cannot create ClaimParty: GetTxOutProof:", err)
		return nil
	}

	client, cleanup, err := ps.GetClient(config.Config.RpcHost)
	if err != nil {
		log.Println("Cannot create ClaimParty: GetClient:", err)
		return nil
	}
	defer cleanup()

	res, err := ps.LiquidGetAddress(client)
	if err != nil {
		log.Println("Cannot create ClaimParty: LiquidGetAddress:", err)
		return nil
	}
	party.Address = res.Address
	party.PubKey = myPublicKey()

	return party
}

// add claim party to the list
func addClaimParty(newParty *ClaimParty) (bool, string) {
	for _, party := range claimParties {
		if party.ClaimScript == newParty.ClaimScript {
			// is already in the list
			return true, "Was already in the list, total participants: " + strconv.Itoa(len(claimParties))
		}
	}

	if len(claimParties) == maxParties {
		return false, "Refused to join, over limit of " + strconv.Itoa(maxParties)
	}

	// verify claimBlockHeight
	proof, err := bitcoin.GetTxOutProof(newParty.TxId)
	if err != nil {
		return false, "Refused to join, TX not confirmed"
	}

	if proof != newParty.TxoutProof {
		log.Printf("New joiner's TxoutProof was wrong")
		newParty.TxoutProof = proof
	}

	txHeight := uint32(internet.GetTxHeight(newParty.TxId))
	claimHight := txHeight + PeginBlocks

	if txHeight > 0 && claimHight != newParty.ClaimBlockHeight {
		log.Printf("New joiner's ClaimBlockHeight was wrong: %d vs %d correct", newParty.ClaimBlockHeight, claimHight)
		newParty.ClaimBlockHeight = claimHight
	}

	claimParties = append(claimParties, newParty)

	// persist to db
	db.Save("ClaimJoin", "claimParties", claimParties)

	return true, "Successfully joined, total participants: " + strconv.Itoa(len(claimParties))
}

// remove claim party from the list by public key
func removeClaimParty(pubKey string) bool {
	var newClaimParties []*ClaimParty
	found := false
	claimBlockHeight := uint32(0)

	for _, party := range claimParties {
		if party.PubKey == pubKey {
			found = true
		} else {
			newClaimParties = append(newClaimParties, party)
			claimBlockHeight = max(claimBlockHeight, party.ClaimBlockHeight)
		}
	}

	if !found {
		return false
	}

	if claimBlockHeight < ClaimBlockHeight {
		ClaimBlockHeight = claimBlockHeight
		// persist to db
		db.Save("ClaimJoin", "ClaimBlockHeight", ClaimBlockHeight)
	}

	claimParties = newClaimParties

	// persist to db
	db.Save("ClaimJoin", "claimParties", claimParties)

	return true
}

func generatePrivateKey() *btcec.PrivateKey {
	privKey, err := btcec.NewPrivateKey()

	if err != nil {
		log.Println("Error generating private key")
		return nil
	}

	return privKey
}

func myPublicKey() string {
	return publicKeyToBase64(myPrivateKey.PubKey())
}

/*
// Payload is encrypted with a new symmetric AES key and returned as base64 string
// The AES key is encrypted using destination secp256k1 pubKey, returned as base64
func encryptPayload(pubKeyString string, plaintext []byte) (string, string, error) {
	// Generate AES key
	aesKey := make([]byte, 32) // 256-bit key
	if _, err := rand.Read(aesKey); err != nil {
		return "", "", err
	}

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", "", err
	}

	nonce := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", "", err
	}

	stream := cipher.NewCTR(block, nonce)
	ciphertext := make([]byte, len(plaintext))
	stream.XORKeyStream(ciphertext, plaintext)

	// Prepend the nonce to the ciphertext
	finalCiphertext := append(nonce, ciphertext...)

	// convert to base64
	base64cypherText := base64.StdEncoding.EncodeToString(finalCiphertext)

	// Encrypt the AES key using RSA public key and returns the encrypted key in base64
	encryptedKey, err := eciesEncrypt(pubKeyString, aesKey)
	if err != nil {
		return "", "", err
	}

	return base64cypherText, encryptedKey, nil
}

*/

// Encrypt with base64 public key
func eciesEncrypt(pubKeyString string, message []byte) (string, error) {

	pubKey, err := base64ToPublicKey(pubKeyString)
	if err != nil {
		return "", err
	}

	ephemeralPrivKey := generatePrivateKey()
	sharedSecret := sha256.Sum256(btcec.GenerateSharedSecret(ephemeralPrivKey, pubKey))

	hkdf := hkdf.New(sha256.New, sharedSecret[:], nil, nil)
	encryptionKey := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(hkdf, encryptionKey); err != nil {
		return "", err
	}

	aead, err := chacha20poly1305.New(encryptionKey)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, chacha20poly1305.NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := aead.Seal(nil, nonce, message, nil)

	result := append(ephemeralPrivKey.PubKey().SerializeCompressed(), nonce...)
	result = append(result, ciphertext...)

	return base64.StdEncoding.EncodeToString(result), nil
}

func eciesDecrypt(privKey *btcec.PrivateKey, ciphertext []byte) ([]byte, error) {
	ephemeralPubKey, err := btcec.ParsePubKey(ciphertext[:33])
	if err != nil {
		return nil, err
	}

	nonce := ciphertext[33 : 33+chacha20poly1305.NonceSize]
	encryptedMessage := ciphertext[33+chacha20poly1305.NonceSize:]

	sharedSecret := sha256.Sum256(btcec.GenerateSharedSecret(privKey, ephemeralPubKey))

	hkdf := hkdf.New(sha256.New, sharedSecret[:], nil, nil)
	encryptionKey := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(hkdf, encryptionKey); err != nil {
		return nil, err
	}

	aead, err := chacha20poly1305.New(encryptionKey)
	if err != nil {
		return nil, err
	}

	decryptedMessage, err := aead.Open(nil, nonce, encryptedMessage, nil)
	if err != nil {
		return nil, err
	}

	return decryptedMessage, nil
}

func publicKeyToBase64(pubKey *btcec.PublicKey) string {
	pubKeyBytes := pubKey.SerializeCompressed() // Compressed format
	return base64.StdEncoding.EncodeToString(pubKeyBytes)
}

func base64ToPublicKey(pubKeyStr string) (*btcec.PublicKey, error) {
	pubKeyBytes, err := base64.StdEncoding.DecodeString(pubKeyStr)
	if err != nil {
		return nil, err
	}
	pubKey, err := btcec.ParsePubKey(pubKeyBytes)
	if err != nil {
		return nil, err
	}
	return pubKey, nil
}

func createClaimPSET(totalFee int) (string, error) {
	// Create the inputs array
	var inputs []map[string]interface{}

	// Create the outputs array
	var outputs []map[string]interface{}

	feeToSplit := totalFee
	feePart := totalFee / len(claimParties)

	// fill in the arrays
	for i, party := range claimParties {
		input := map[string]interface{}{
			"txid":               party.TxId,
			"vout":               party.Vout,
			"pegin_bitcoin_tx":   party.RawTx,
			"pegin_txout_proof":  party.TxoutProof,
			"pegin_claim_script": party.ClaimScript,
		}

		if i == len(claimParties)-1 {
			// the last joiner pays higher fee if it cannot be divided equally
			feePart = feeToSplit
		}

		output := map[string]interface{}{
			party.Address:   liquid.ToBitcoin(party.Amount - uint64(feePart)),
			"blinder_index": i,
		}

		feeToSplit -= feePart
		inputs = append(inputs, input)
		outputs = append(outputs, output)
	}

	// shuffle the outputs
	for i := len(outputs) - 1; i > 0; i-- {
		j := mathRand.Intn(i + 1)
		outputs[i], outputs[j] = outputs[j], outputs[i]
	}

	// add total fee output
	outputs = append(outputs, map[string]interface{}{
		"fee": liquid.ToBitcoin(uint64(totalFee)),
	})

	// add op_return
	outputs = append(outputs, map[string]interface{}{
		"data": "6a0f506565725377617020576562205549",
	})

	// Combine inputs and outputs into the parameters array
	params := []interface{}{inputs, outputs}

	return liquid.CreatePSET(params)
}

// Serialize btcec.PrivateKey and save to db
func savePrivateKey() {
	if myPrivateKey == nil {
		return
	}
	data := myPrivateKey.Serialize()
	db.Save("ClaimJoin", "serializedPrivateKey", data)
}

// checks that the output includes my address and amount
func verifyPSET() bool {
	decoded, err := liquid.DecodePSET(claimPSET)
	if err != nil {
		return false
	}

	for _, output := range decoded.Outputs {
		// 50 sats maximum fee allowed
		if output.Script.Address == claimParties[0].Address && liquid.ToBitcoin(claimParties[0].Amount)-output.Amount < 0.0000005 {
			return true
		}
	}
	return false
}
