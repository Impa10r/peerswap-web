package ln

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/gob"
	"io"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	mathRand "math/rand"

	"github.com/btcsuite/btcd/btcec/v2"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"

	"peerswap-web/cmd/psweb/bitcoin"
	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/db"
	"peerswap-web/cmd/psweb/liquid"
	"peerswap-web/cmd/psweb/ps"
)

// maximum number of participants in ClaimJoin
const MAX_PARTIES = 10

var (
	// encryption private key
	myPrivateKey *btcec.PrivateKey
	// maps learned public keys to node Id
	keyToNodeId = make(map[string]string)
	// public key of the sender of peg-in_started broadcast
	ClaimJoinHandler string
	// timestamp of the peg-in_started broadcast of the current ClaimJoinHandler
	ClaimJoinHandlerTS uint64
	// Peg-in txid of the ClaimJoinHandler for rebroadcasts
	ClaimJoinHandlerTxId string
	// when currently pending peg-in can be claimed
	ClaimBlockHeight uint32
	// time limit to join another claim
	JoinBlockHeight uint32
	// human readable status of the claimjoin
	ClaimStatus = "No ClaimJoin peg-in is pending"
	// none, initiator or joiner
	MyRole = "none"
	// array of initiator + joiners, for initiator only
	ClaimParties []ClaimParty
	// PSET to be blinded and signed by all parties
	claimPSET string
	// count how many times tried to join, to give up after 10
	joinCounter int
)

type Coordination struct {
	// possible actions: add, confirm_add, refuse_add, remove, process, process2
	Action string
	// new joiner details
	Joiner ClaimParty
	// ETA of currently pending peg-in claim
	ClaimBlockHeight uint32
	// human readable status of the claimjoin
	Status string
	// partially signed elements transaction
	PSET []byte
}

type ClaimParty struct {
	// peg-in txid
	TxId string
	// peg-in vout
	Vout uint
	// peg-in claim script
	ClaimScript string
	// Liquid address to receive funds
	Address string
	// when can be claimed
	ClaimBlockHeight uint32
	// to be filled locally by initiator
	RawTx      string
	TxoutProof string
	Amount     uint64
	FeeShare   uint64
	PubKey     string
	SentCount  uint
	SentTime   time.Time
}

// runs after restart, to continue if peg-in is ongoing
func loadClaimJoinDB() {
	db.Load("ClaimJoin", "ClaimJoinHandler", &ClaimJoinHandler)
	db.Load("ClaimJoin", "ClaimJoinHandlerTS", &ClaimJoinHandlerTS)
	db.Load("ClaimJoin", "ClaimBlockHeight", &ClaimBlockHeight)
	db.Load("ClaimJoin", "JoinBlockHeight", &JoinBlockHeight)
	db.Load("ClaimJoin", "ClaimStatus", &ClaimStatus)

	db.Load("ClaimJoin", "MyRole", &MyRole)
	db.Load("ClaimJoin", "keyToNodeId", &keyToNodeId)
	db.Load("ClaimJoin", "ClaimParties", &ClaimParties)

	if MyRole != "none" {
		if config.Config.PeginTxId == "" {
			// was claimed already
			resetClaimJoin()
			return
		}

		var serializedKey []byte
		db.Load("ClaimJoin", "serializedPrivateKey", &serializedKey)
		myPrivateKey, _ = btcec.PrivKeyFromBytes(serializedKey)
		log.Println("Continue as", MyRole, MyPublicKey())

		if MyRole == "initiator" {
			db.Load("ClaimJoin", "claimPSET", &claimPSET)
		}
	} else if ClaimJoinHandler != "" {
		log.Println("Continue with ClaimJoin invite from", ClaimJoinHandler)
	}
}

// runs every block
func OnBlock(blockHeight uint32) {
	if !config.Config.PeginClaimJoin || MyRole != "initiator" || blockHeight < ClaimBlockHeight {
		return
	}

	// initial fee estimate
	totalFee := 40 + 30*(len(ClaimParties)-1)
	errorCounter := 0

create_pset:
	if claimPSET == "" {
		var err error
		claimPSET, err = createClaimPSET(totalFee)
		if err != nil {
			// claimjoin failed
			EndClaimJoin("", err.Error())
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

	// verify number of inputs and outputs
	numOutputs := len(ClaimParties) + 1
	if numOutputs > 2 {
		numOutputs++ // add op_return
	}

	if len(analyzed.Outputs) != numOutputs || len(decoded.Inputs) != len(ClaimParties) {
		log.Printf("Malformed PSET with %d inputs and %d outputs, trying again", len(decoded.Inputs), len(analyzed.Outputs))
		claimPSET = ""
		db.Save("ClaimJoin", "claimPSET", &claimPSET)

		errorCounter++
		if errorCounter < 10 {
			goto create_pset
		}
		return
	}

	total := strconv.Itoa(len(ClaimParties))

	for i, output := range analyzed.Outputs {
		if output.Blind && output.Status == "unblinded" {
			blinder := decoded.Outputs[i].BlinderIndex
			ClaimStatus = "Blinding " + strconv.Itoa(i+1) + "/" + total

			if blinder == 0 {
				// my output
				log.Println(ClaimStatus)
				claimPSET, _, err = liquid.ProcessPSET(claimPSET)
				if err != nil {
					log.Println("Unable to blind output, cancelling ClaimJoin:", err)
					EndClaimJoin("", "Coordination failure")
					return
				}
				ClaimStatus += " done"
				log.Println(ClaimStatus)
			} else {
				action := "process"
				if i == len(ClaimParties)-1 {
					// the final blinder can blind and sign at once
					action = "process2"
					ClaimStatus += " & Signing 1/" + total
				}

				serializedPset, err := base64.StdEncoding.DecodeString(claimPSET)
				if err != nil {
					log.Println("Unable to serialize PSET")
					return
				}

				if SendCoordination(ClaimParties[blinder].PubKey, &Coordination{
					Action:           action,
					PSET:             serializedPset,
					Status:           ClaimStatus,
					ClaimBlockHeight: ClaimBlockHeight,
				}, true) {
					log.Println(ClaimStatus)
					db.Save("ClaimJoin", "ClaimStatus", &ClaimStatus)
				}

				return
			}
		}
	}

	// Iterate through inputs in reverse order to sign
	for i := len(ClaimParties) - 1; i >= 0; i-- {
		input := decoded.Inputs[i]

		signing := 1
		if strings.HasSuffix(ClaimStatus, "& Signing 1/"+total+" done") {
			signing = 2
		} else if strings.HasPrefix(ClaimStatus, "Signing") && strings.HasSuffix(ClaimStatus, total+" done") {
			// Define a regex pattern to find the first number
			re := regexp.MustCompile(`\d+`)
			match := re.FindString(ClaimStatus)

			// Convert the matched string to an integer
			if match != "" {
				num, err := strconv.Atoi(match)
				if err == nil {
					signing = num + 1
				}
			}
		}

		if len(input.FinalScriptWitness) == 0 {
			ClaimStatus = "Signing " + strconv.Itoa(signing) + "/" + total

			if i == 0 {
				// my input, last to sign
				log.Println(ClaimStatus)
				claimPSET, _, err = liquid.ProcessPSET(claimPSET)
				if err != nil {
					log.Println("Unable to sign input, cancelling ClaimJoin:", err)
					EndClaimJoin("", "Initiator signing failure")
					return
				}
				ClaimStatus += " done"
				log.Println(ClaimStatus)
				db.Save("ClaimJoin", "ClaimStatus", &ClaimStatus)
			} else {
				serializedPset, err := base64.StdEncoding.DecodeString(claimPSET)
				if err != nil {
					log.Println("Unable to serialize PSET")
					return
				}

				if SendCoordination(ClaimParties[i].PubKey, &Coordination{
					Action:           "process",
					PSET:             serializedPset,
					Status:           ClaimStatus,
					ClaimBlockHeight: ClaimBlockHeight,
				}, true) {
					log.Println(ClaimStatus)
					db.Save("ClaimJoin", "ClaimStatus", &ClaimStatus)

				}
				return
			}
		}
	}

	// analyze again after I sign
	analyzed, err = liquid.AnalyzePSET(claimPSET)
	if err != nil {
		return
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

		if decodedTx.DiscountVsize == 0 {
			log.Println("Decoded transaction omits DiscountVsize, cancelling ClaimJoin")
			EndClaimJoin("", "Final TX failure: no CT discount")
			return
		}

		exactFee := (decodedTx.DiscountVsize / 10)
		if decodedTx.DiscountVsize%10 != 0 {
			exactFee++
		}

		var feeValue int
		found := false

		// Iterate over the map
		for _, value := range decodedTx.Fee {
			feeValue = int(toSats(value))
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
			totalFee = exactFee
			ClaimStatus = "Redo to improve fee"
			claimPSET = ""

			db.Save("ClaimJoin", "claimPSET", &claimPSET)
			db.Save("ClaimJoin", "ClaimStatus", &ClaimStatus)

			errorCounter = 0
			goto create_pset

		} else {
			// post raw transaction
			log.Println("Posting final TX")

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

func kickPeer(pubKey, reason string) {
	if ok := removeClaimParty(pubKey); ok {
		ClaimStatus = "Kicked out " + pubKey + ", total participants: " + strconv.Itoa(len(ClaimParties))
		// persist to db
		db.Save("ClaimJoin", "ClaimBlockHeight", ClaimBlockHeight)
		log.Println(ClaimStatus, "for", reason)
		// erase PSET to start over
		claimPSET = ""
		// persist to db
		db.Save("ClaimJoin", "claimPSET", claimPSET)
		// inform the offender
		SendCoordination(pubKey, &Coordination{
			Action: "refuse_add",
			Status: "Kicked out for " + reason,
		}, false)
		// inform others
		sendToGroup("One peer was kicked out, total participants: " + strconv.Itoa(len(ClaimParties)))
		return
	}

	EndClaimJoin("", "Coordination failure")
}

// Called when received a broadcast custom message
// Forward the message to all direct peers, unless the source key is known already
// (it means the message came back to you from a downstream peer)
func Broadcast(fromNodeId string, message *Message) bool {

	sent := false

	if fromNodeId == MyNodeId || (fromNodeId != MyNodeId && (message.Asset == "pegin_started" && keyToNodeId[message.Sender] == "" || message.Asset == "pegin_ended" && keyToNodeId[message.Sender] != "")) {
		// forward to everyone else
		client, cleanup, err := ps.GetClient(config.Config.RpcHost)
		if err != nil {
			return false
		}
		defer cleanup()

		res, err := ps.ListPeers(client)
		if err != nil {
			return false
		}

		for _, peer := range res.GetPeers() {
			// don't send it back to where it came from
			if peer.NodeId != fromNodeId {
				if SendCustomMessage(peer.NodeId, message) == nil {
					sent = true
				}
			}
		}
	}

	if fromNodeId == MyNodeId || message.Sender == MyPublicKey() {
		// do nothing more if this is my own broadcast
		return sent
	}

	if keyToNodeId[message.Sender] == "" {
		// store path for relaying further encrypted messages
		keyToNodeId[message.Sender] = fromNodeId
		db.Save("ClaimJoin", "keyToNodeId", keyToNodeId)
	}

	// react to received broadcast
	switch message.Asset {
	case "pegin_started":
		if MyRole == "initiator" {
			// two simultaneous initiators conflict, the earlier wins
			if len(ClaimParties) > 1 || ClaimJoinHandlerTS < message.TimeStamp {
				log.Println("Initiator collision, staying as initiator")
				// repeat peg-in start info
				SendCustomMessage(fromNodeId, &Message{
					Version:   MESSAGE_VERSION,
					Memo:      "broadcast",
					Asset:     "pegin_started",
					Amount:    uint64(JoinBlockHeight),
					Sender:    MyPublicKey(),
					TimeStamp: ClaimJoinHandlerTS,
					Payload:   []byte(config.Config.PeginTxId),
				})
				return false
			} else {
				log.Println("Initiator collision, switching to 'none'")
				MyRole = "none"
				ClaimJoinHandler = ""
				db.Save("ClaimJoin", "MyRole", MyRole)
				db.Save("ClaimJoin", "ClaimJoinHandler", ClaimJoinHandler)
			}
		} else if MyRole == "joiner" {
			// already joined another group, ignore
			return false
		}

		if ClaimJoinHandler == "" {
			// verify that peg-in has indeed started
			_, err := bitcoin.GetTxOutProof(string(message.Payload))
			if err != nil {
				// try again
				time.Sleep(10 * time.Second)
				_, err = bitcoin.GetTxOutProof(string(message.Payload))
			}
			if err != nil {
				log.Println("Failed to get Initiator's TxOutProof, ignoring invite")
				return false
			}

			ClaimJoinHandler = message.Sender
			ClaimJoinHandlerTS = message.TimeStamp
			ClaimJoinHandlerTxId = string(message.Payload)
			// Time limit to apply is communicated via Amount
			JoinBlockHeight = uint32(message.Amount)
			// reset counter of join attempts
			joinCounter = 0

			ClaimStatus = "Received invitation to ClaimJoin"

			log.Println(ClaimStatus, "from", ClaimJoinHandler, "via", GetAlias(fromNodeId))

			// persist to db
			db.Save("ClaimJoin", "ClaimJoinHandler", ClaimJoinHandler)
			db.Save("ClaimJoin", "ClaimJoinHandlerTS", ClaimJoinHandlerTS)
			db.Save("ClaimJoin", "JoinBlockHeight", JoinBlockHeight)
			db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)
		}

	case "pegin_ended":
		// only trust the message from the original handler
		if ClaimJoinHandler == message.Sender {
			txId := string(message.Payload)
			if MyRole == "joiner" && txId != "" && config.Config.PeginClaimScript != "done" && len(ClaimParties) == 1 {
				var decoded liquid.Transaction
				var err error

				// wait 60 seconds for the posted tx to appear in our local mempool
				timeout := time.After(60 * time.Second)
				ticker := time.NewTicker(5 * time.Second) // Check every 5 seconds

				for {
					select {
					case <-timeout:
						ClaimStatus = "Transaction did not appear in mempool within 60 seconds"
						log.Println("Transaction did not appear in mempool within 60 seconds:", err)
						ticker.Stop()
						return false
					case <-ticker.C:
						_, err = liquid.GetRawTransaction(txId, &decoded)
						if err == nil {
							// Transaction found, proceed
							ticker.Stop()
							goto FoundTransaction
						}
					}
				}

			FoundTransaction:
				addressInfo, err := liquid.GetAddressInfo(ClaimParties[0].Address)
				if err != nil {
					return false
				}

				// verify that my address was funded
				ok := false
				for _, output := range decoded.Vout {
					if output.ScriptPubKey.Address == addressInfo.Unconfidential {
						ok = true
						break
					}
				}
				if ok {
					ClaimStatus = "ClaimJoin pegin complete! Liquid TxId: " + txId
					// signal to telegram bot
					config.Config.PeginTxId = txId
					config.Config.PeginClaimScript = "done"
				} else {
					ClaimStatus = "My liquid address not found in the posted transaction"
				}
			} else {
				ClaimStatus = "Invitation to ClaimJoin revoked"
			}
			log.Println(ClaimStatus)
			resetClaimJoin()
		} else {
			// forget the route only
			keyToNodeId[message.Sender] = ""
			db.Save("ClaimJoin", "keyToNodeId", keyToNodeId)
		}
	}

	return false
}

// Encrypt and send message to an anonymous peer identified by base64 public key
// New keys are generated at the start of each ClaimJoin session
// Peers track sources of encrypted messages to forward back the replies
func SendCoordination(destinationPubKey string, message *Coordination, needsResponse bool) bool {

	destinationNodeId := keyToNodeId[destinationPubKey]
	if destinationNodeId == "" {
		log.Println("Cannot send coordination, destination PubKey has no matching NodeId")
		return false
	}

	// Serialize the message using gob
	var buffer bytes.Buffer
	encoder := gob.NewEncoder(&buffer)
	if err := encoder.Encode(message); err != nil {
		log.Println("Cannot encode with GOB:", err)
		return false
	}

	// Encrypt the message using the base64 receiver's public key
	ciphertext, err := eciesEncrypt(destinationPubKey, buffer.Bytes())
	if err != nil {
		log.Println("Error encrypting message:", err)
		return false
	}

	partyN := 0
	for i := 1; i < len(ClaimParties); i++ {
		if ClaimParties[i].PubKey == destinationPubKey {
			partyN = i
			break
		}
	}

	if needsResponse && time.Since(ClaimParties[partyN].SentTime) < 2*time.Second {
		// resend too soon, better wait for reply
		return false
	}

	err = SendCustomMessage(destinationNodeId, &Message{
		Version:     MESSAGE_VERSION,
		Memo:        "process",
		Sender:      MyPublicKey(),
		Destination: destinationPubKey,
		Payload:     ciphertext,
	})

	if err != nil {
		log.Println("Cannot send custom message:", err)
		return false
	}

	if needsResponse && partyN > 0 {
		// allow maximum 5 resends without a response
		ClaimParties[partyN].SentCount++
		if ClaimParties[partyN].SentCount > 4 {
			// peer is not responding, kick him
			kickPeer(destinationPubKey, "being unresponsive")
			return false
		}
		// remember when last sent a message requiring response
		ClaimParties[partyN].SentTime = time.Now()
	}
	return true
}

// Either forward to final destination or decrypt and process
func Process(message *Message, senderNodeId string) {
	if keyToNodeId[message.Sender] == "" && senderNodeId != MyNodeId {
		// save source key map
		keyToNodeId[message.Sender] = senderNodeId
		// persist to db
		db.Save("ClaimJoin", "keyToNodeId", keyToNodeId)
	}

	if message.Destination == MyPublicKey() {
		// find the peer
		for i := 1; i < len(ClaimParties); i++ {
			if ClaimParties[i].PubKey == message.Sender {
				// reset send conter
				ClaimParties[i].SentCount = 0
				break
			}
		}

		if config.Config.PeginClaimJoin && len(ClaimParties) > 0 {
			// Decrypt the message using my private key
			plaintext, err := eciesDecrypt(myPrivateKey, message.Payload)
			if err != nil {
				log.Printf("Error decrypting payload: %s", err)
				return
			}

			// recover the struct
			var msg Coordination
			var buffer bytes.Buffer

			// Write the byte slice into the buffer
			buffer.Write(plaintext)

			// Deserialize binary data
			decoder := gob.NewDecoder(&buffer)
			if err := decoder.Decode(&msg); err != nil {
				log.Printf("Received an incorrectly formed Coordination: %s", err)
				return
			}

			switch msg.Action {
			case "add":
				if MyRole != "initiator" {
					log.Printf("Cannot add a peer, not a claim initiator")
					SendCoordination(msg.Joiner.PubKey, &Coordination{
						Action: "refuse_add",
						Status: "Cannot add, no longer a claim initiator",
					}, false)
					return
				}

				if ok, status := addClaimParty(&msg.Joiner); ok {
					if SendCoordination(msg.Joiner.PubKey, &Coordination{
						Action:           "confirm_add",
						ClaimBlockHeight: max(ClaimBlockHeight, msg.ClaimBlockHeight),
						Status:           status,
					}, false) {
						ClaimBlockHeight = max(ClaimBlockHeight, msg.ClaimBlockHeight)
						db.Save("ClaimJoin", "ClaimBlockHeight", ClaimBlockHeight)

						ClaimStatus = "Added new peer, total participants: " + strconv.Itoa(len(ClaimParties))
						db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)
						log.Println("Added "+msg.Joiner.PubKey+", total:", len(ClaimParties))
						sendToGroup("Another peer joined, total participants: " + strconv.Itoa(len(ClaimParties)))
					}
				} else {
					if SendCoordination(msg.Joiner.PubKey, &Coordination{
						Action: "refuse_add",
						Status: status,
					}, false) {
						log.Println("Refused new peer: ", status)
					}
				}

			case "remove":
				if MyRole != "initiator" {
					log.Printf("Cannot remove a peer, not a claim initiator")
					SendCoordination(msg.Joiner.PubKey, &Coordination{
						Action: "refuse_add",
						Status: "Cannot remove, not a claim initiator",
					}, false)
					return
				}

				if removeClaimParty(msg.Joiner.PubKey) {
					ClaimStatus = "Removed a peer, total participants: " + strconv.Itoa(len(ClaimParties))
					// persist to db
					db.Save("ClaimJoin", "ClaimBlockHeight", ClaimBlockHeight)
					log.Println(ClaimStatus)
					// erase PSET to start over
					claimPSET = ""
					// persist to db
					db.Save("ClaimJoin", "claimPSET", claimPSET)
					sendToGroup("One peer left, total participants: " + strconv.Itoa(len(ClaimParties)))
				} else {
					log.Println("Cannot remove peer, not in the list")
				}

			case "confirm_add":
				ClaimBlockHeight = msg.ClaimBlockHeight
				ClaimJoinHandler = message.Sender
				MyRole = "joiner"
				ClaimStatus = msg.Status
				log.Println(ClaimStatus)
				// persist to db
				db.Save("ClaimJoin", "MyRole", MyRole)
				db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)
				db.Save("ClaimJoin", "ClaimBlockHeight", ClaimBlockHeight)
				db.Save("ClaimJoin", "ClaimJoinHandler", ClaimJoinHandler)

			case "refuse_add":
				log.Println(msg.Status)
				// forget pegin handler, for not to try joining it again
				forgetPubKey(ClaimJoinHandler)
				ClaimStatus = msg.Status

			case "process2": // process twice to blind and sign
				if MyRole != "joiner" {
					log.Println("Received process2 while not a joiner")
					return
				}

				// process my output
				newClaimPSET := base64.StdEncoding.EncodeToString(msg.PSET)
				newClaimPSET, _, err = liquid.ProcessPSET(newClaimPSET)
				if err != nil {
					log.Println("Unable to encode PSET:", err)
					return
				}

				// save back into message
				msg.PSET, err = base64.StdEncoding.DecodeString(newClaimPSET)
				if err != nil {
					log.Println("Unable to decode PSET:", err)
					return
				}

				fallthrough // continue to second pass

			case "process": // blind or sign
				// if verified successfully, saves the new PSET as claimPSET
				if !verifyPSET(base64.StdEncoding.EncodeToString(msg.PSET)) {
					log.Println("PSET verification failure!")
					if MyRole == "initiator" {
						// kick the joiner who returned broken PSET
						kickPeer(message.Sender, "invalid PSET return")
						return
					} else {
						// remove yourself from ClaimJoin
						if SendCoordination(ClaimJoinHandler, &Coordination{
							Action: "remove",
							Joiner: ClaimParties[0],
						}, false) {
							// forget pegin handler, so that cannot initiate new ClaimJoin
							JoinBlockHeight = 0
							ClaimJoinHandler = ""
							ClaimStatus = "Left ClaimJoin group"
							MyRole = "none"
							log.Println(ClaimStatus)

							db.Save("ClaimJoin", "MyRole", &MyRole)
							db.Save("ClaimJoin", "ClaimStatus", &ClaimStatus)
							db.Save("ClaimJoin", "ClaimJoinHandler", &ClaimJoinHandler)
						}
					}
					return
				}

				if MyRole == "initiator" {
					if msg.Status != ClaimStatus+" done" {
						// not the expected reply, ignore
						return
					}

					ClaimStatus = msg.Status
					log.Println(ClaimStatus)

					db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)

					// Save the received claimPSET
					db.Save("ClaimJoin", "claimPSET", &claimPSET)

					// execute onBlock to continue signing
					OnBlock(ClaimBlockHeight)
					return
				}

				if MyRole != "joiner" {
					log.Println("Received 'process' unexpected")
					return
				}

				// process my output
				claimPSET, _, err = liquid.ProcessPSET(claimPSET)
				if err != nil {
					log.Println("Unable to process PSET:", err)
					return
				}

				ClaimBlockHeight = msg.ClaimBlockHeight
				ClaimStatus = msg.Status + " done"
				log.Println(ClaimStatus)

				db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)
				db.Save("ClaimJoin", "ClaimBlockHeight", ClaimBlockHeight)

				serializedPset, err := base64.StdEncoding.DecodeString(claimPSET)
				if err != nil {
					log.Println("Unable to serialize PSET")
					return
				}

				// return PSET to Handler
				if !SendCoordination(ClaimJoinHandler, &Coordination{
					Action: "process",
					PSET:   serializedPset,
					Status: ClaimStatus,
				}, false) {
					log.Println("Unable to send coordination, cancelling ClaimJoin")
					EndClaimJoin("", "Coordination failure")
				}
			}
		}
		return
	}

	destinationNodeId := keyToNodeId[message.Destination]
	if destinationNodeId == "" {
		log.Println("Cannot relay: destination PubKey " + message.Destination + " has no matching NodeId")
		// forget the pubKey
		forgetPubKey(message.Destination)
		// inform the sender that was unable to relay
		SendCustomMessage(senderNodeId, &Message{
			Version:     MESSAGE_VERSION,
			Memo:        "unable",
			Destination: message.Destination,
			Sender:      MyPublicKey(),
		})
		return
	}

	log.Println("Relaying", message.Memo, "from", GetAlias(senderNodeId), "to", GetAlias(destinationNodeId))

	err := SendCustomMessage(destinationNodeId, message)
	if err != nil {
		log.Println("Cannot relay:", err)
	}

}

// pass information for all members of the group
func sendToGroup(status string) {
	if len(ClaimParties) > 2 {
		// inform the other joiners of the new ClaimBlockHeight
		for i := 1; i <= len(ClaimParties)-2; i++ {
			SendCoordination(ClaimParties[i].PubKey, &Coordination{
				Action:           "confirm_add",
				ClaimBlockHeight: ClaimBlockHeight,
				Status:           status,
			}, false)
		}
	}
}

// no message route to destination
func forgetPubKey(destination string) {
	// destination pubkey was invalid
	if ClaimJoinHandler == destination {
		if MyRole == "joiner" {
			MyRole = "none"
			ClaimStatus = "Unable to contact Initiator, resetting"
			log.Println(ClaimStatus)
			db.Save("ClaimJoin", "MyRole", MyRole)
			db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)
		}
		ClaimJoinHandler = ""
		db.Save("ClaimJoin", "ClaimJoinHandler", ClaimJoinHandler)
	}
	keyToNodeId[destination] = ""
	db.Save("ClaimJoin", "keyToNodeId", keyToNodeId)
}

// called for claim join initiator after his pegin funding tx confirms
func InitiateClaimJoin(claimBlockHeight uint32) bool {
	if myPrivateKey == nil {
		myPrivateKey = generatePrivateKey()
		if myPrivateKey != nil {
			// persist to db
			savePrivateKey()
		} else {
			return false
		}
	}

	// original invitation timestamp
	ts := ClaimJoinHandlerTS
	if MyRole == "none" && len(ClaimParties) != 1 || ClaimParties[0].PubKey != MyPublicKey() {
		party := createClaimParty(claimBlockHeight)
		if party != nil {
			// initiate array of claim parties
			ClaimParties = nil
			ClaimParties = append(ClaimParties, *party)
			ClaimBlockHeight = claimBlockHeight
			JoinBlockHeight = claimBlockHeight - 1
			db.Save("ClaimJoin", "ClaimBlockHeight", ClaimBlockHeight)
			db.Save("ClaimJoin", "JoinBlockHeight", JoinBlockHeight)
			db.Save("ClaimJoin", "ClaimParties", ClaimParties)
			// new invitation timestamp
			ts = uint64(time.Now().Unix())
		} else {
			return false
		}
	}

	if Broadcast(MyNodeId, &Message{
		Version:   MESSAGE_VERSION,
		Memo:      "broadcast",
		Asset:     "pegin_started",
		Amount:    uint64(JoinBlockHeight),
		Sender:    MyPublicKey(),
		TimeStamp: ts,
		Payload:   []byte(config.Config.PeginTxId),
	}) {
		// at least one peer received it
		if len(ClaimParties) == 1 {
			// initial invite before everyone joined
			ClaimJoinHandlerTS = ts
			ClaimStatus = "Invites sent, awaiting peers to join"
			// persist to db
			db.Save("ClaimJoin", "ClaimJoinHandlerTS", ClaimJoinHandlerTS)
			db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)
		}
		return true
	}
	return false
}

// called by claim join initiator after posting claim tx or on error
func EndClaimJoin(txId string, status string) bool {
	Broadcast(MyNodeId, &Message{
		Version:     MESSAGE_VERSION,
		Memo:        "broadcast",
		Asset:       "pegin_ended",
		Amount:      uint64(ClaimBlockHeight),
		Sender:      MyPublicKey(),
		Payload:     []byte(txId),
		Destination: status,
	})

	if txId != "" {
		log.Println("ClaimJoin peg-in complete! Liquid TxId:", txId)
		// signal to telegram bot
		config.Config.PeginTxId = txId
		config.Config.PeginClaimScript = "done"
	}

	resetClaimJoin()
	return true
}

func resetClaimJoin() {
	// eraze all traces
	ClaimBlockHeight = 0
	JoinBlockHeight = 0
	ClaimParties = nil
	MyRole = "none"
	ClaimJoinHandler = ""
	ClaimStatus = "No ClaimJoin peg-in is pending"
	keyToNodeId = make(map[string]string)

	// persist to db
	db.Save("ClaimJoin", "ClaimParties", ClaimParties)
	db.Save("ClaimJoin", "ClaimJoinHandler", ClaimJoinHandler)
	db.Save("ClaimJoin", "ClaimBlockHeight", ClaimBlockHeight)
	db.Save("ClaimJoin", "JoinBlockHeight", JoinBlockHeight)
	db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)
	db.Save("ClaimJoin", "MyRole", MyRole)
	db.Save("ClaimJoin", "keyToNodeId", keyToNodeId)
}

// called for ClaimJoin joiner candidate after his pegin funding tx confirms
func JoinClaimJoin(claimBlockHeight uint32) bool {
	if ClaimJoinHandler == "" {
		return false
	}

	if joinCounter > 2 {
		// no reply, remove yourself from ClaimJoin
		SendCoordination(ClaimJoinHandler, &Coordination{
			Action: "remove",
			Joiner: ClaimParties[0],
		}, false)
		forgetPubKey(ClaimJoinHandler)
		ClaimStatus = "Initator does not respond, forget him"

		// poll to find out a new ClaimJoinHandler
		client, cleanup, err := ps.GetClient(config.Config.RpcHost)
		if err != nil {
			return false
		}
		defer cleanup()

		res, err := ps.ListPeers(client)
		if err != nil {
			return false
		}

		for _, peer := range res.GetPeers() {
			SendCustomMessage(peer.NodeId, &Message{
				Version: MESSAGE_VERSION,
				Memo:    "poll",
			})
		}

		return false
	}

	// preserve the same key for several join attempts
	if myPrivateKey == nil {
		myPrivateKey = generatePrivateKey()
		if myPrivateKey != nil {
			// persist to db
			savePrivateKey()
		} else {
			return false
		}
	}

	if len(ClaimParties) != 1 || ClaimParties[0].PubKey != MyPublicKey() {
		// initiate array of claim parties for single entry
		ClaimParties = nil
		cp := createClaimParty(claimBlockHeight)
		if cp == nil {
			// something went wrong
			return false
		}
		ClaimParties = append(ClaimParties, *cp)
		ClaimBlockHeight = claimBlockHeight
		db.Save("ClaimJoin", "ClaimBlockHeight", ClaimBlockHeight)
		db.Save("ClaimJoin", "ClaimParties", ClaimParties)
	}

	if SendCoordination(ClaimJoinHandler, &Coordination{
		Action:           "add",
		Joiner:           ClaimParties[0],
		ClaimBlockHeight: claimBlockHeight,
	}, false) {
		// increment counter
		joinCounter++
		ClaimStatus = "Responded to invitation, awaiting confirmation"
		// persist to db
		db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)
		return true
	}

	return false
}

func shareInvite(nodeId string) {
	sender := ClaimJoinHandler
	if MyRole == "initiator" {
		sender = MyPublicKey()
	}
	if sender != "" && GetBlockHeight() < JoinBlockHeight {
		// repeat pegin start info
		SendCustomMessage(nodeId, &Message{
			Version:   MESSAGE_VERSION,
			Memo:      "broadcast",
			Asset:     "pegin_started",
			Amount:    uint64(JoinBlockHeight),
			Sender:    sender,
			TimeStamp: ClaimJoinHandlerTS,
			Payload:   []byte(ClaimJoinHandlerTxId),
		})
	}
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
		// try again
		time.Sleep(2 * time.Second)
		party.TxoutProof, err = bitcoin.GetTxOutProof(config.Config.PeginTxId)
	}
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
	party.PubKey = MyPublicKey()

	return party
}

// add claim party to the list
func addClaimParty(newParty *ClaimParty) (bool, string) {

	for _, party := range ClaimParties {
		if party.ClaimScript == newParty.ClaimScript {
			// is already in the list
			return true, "Successfully joined, total participants: " + strconv.Itoa(len(ClaimParties))
		}
	}

	if len(ClaimParties) == MAX_PARTIES {
		return false, "Refuse to add, over limit of " + strconv.Itoa(MAX_PARTIES)
	}

	// verify TxOutProof
	proof, err := bitcoin.GetTxOutProof(newParty.TxId)
	if err != nil {
		return false, "Refuse to add, TX not confirmed"
	}

	if proof != newParty.TxoutProof {
		log.Printf("New peer's TxoutProof was wrong")
		newParty.TxoutProof = proof
	}

	ClaimParties = append(ClaimParties, *newParty)

	// persist to db
	db.Save("ClaimJoin", "ClaimParties", ClaimParties)

	return true, "Successfully joined, total participants: " + strconv.Itoa(len(ClaimParties))
}

// remove claim party from the list by public key
func removeClaimParty(pubKey string) bool {
	var newClaimParties []ClaimParty
	found := false
	claimBlockHeight := uint32(0)

	for _, party := range ClaimParties {
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

	ClaimParties = newClaimParties

	// persist to db
	db.Save("ClaimJoin", "ClaimParties", ClaimParties)

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

func MyPublicKey() string {
	if myPrivateKey == nil {
		myPrivateKey = generatePrivateKey()
		if myPrivateKey != nil {
			// persist to db
			savePrivateKey()
		}
	}
	return publicKeyToBase64(myPrivateKey.PubKey())
}

// Encrypt with base64 public key
func eciesEncrypt(pubKeyString string, message []byte) ([]byte, error) {

	pubKey, err := base64ToPublicKey(pubKeyString)
	if err != nil {
		return nil, err
	}

	ephemeralPrivKey := generatePrivateKey()
	sharedSecret := sha256.Sum256(btcec.GenerateSharedSecret(ephemeralPrivKey, pubKey))

	hkdf := hkdf.New(sha256.New, sharedSecret[:], nil, nil)
	encryptionKey := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(hkdf, encryptionKey); err != nil {
		return nil, err
	}

	aead, err := chacha20poly1305.New(encryptionKey)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, chacha20poly1305.NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	ciphertext := aead.Seal(nil, nonce, message, nil)

	result := append(ephemeralPrivKey.PubKey().SerializeCompressed(), nonce...)
	result = append(result, ciphertext...)

	return result, nil
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
	feePart := totalFee / len(ClaimParties)

	// fill in the arrays
	for i, party := range ClaimParties {
		input := map[string]interface{}{
			"txid":               party.TxId,
			"vout":               party.Vout,
			"pegin_bitcoin_tx":   party.RawTx,
			"pegin_txout_proof":  party.TxoutProof,
			"pegin_claim_script": party.ClaimScript,
		}

		if i == len(ClaimParties)-1 {
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

	if len(ClaimParties) > 1 {
		// add op_return
		outputs = append(outputs, map[string]interface{}{
			"data": "506565725377617020576562205549",
		})
	}

	// Combine inputs and outputs into the parameters array
	return liquid.CreatePSET([]interface{}{inputs, outputs})
}

// Serialize btcec.PrivateKey and save to db
func savePrivateKey() {
	if myPrivateKey == nil {
		return
	}
	data := myPrivateKey.Serialize()
	db.Save("ClaimJoin", "serializedPrivateKey", data)
}

// checks that the new PSET has the same input/output count
// checks that outputs include my address and correct amount
func verifyPSET(newClaimPSET string) bool {
	decodedNew, err := liquid.DecodePSET(newClaimPSET)
	if err != nil {
		return false
	}

	if MyRole == "initiator" {
		if newClaimPSET == claimPSET {
			log.Println("Peer returned identical PSET")
			return false
		}

		decodedOld, err := liquid.DecodePSET(claimPSET)
		if err != nil {
			return false
		}

		if decodedOld.InputCount != decodedNew.InputCount {
			log.Println("PSET verification failed: wrong InputCount")
			return false
		}

		if decodedOld.OutputCount != decodedNew.OutputCount {
			log.Println("PSET verification failed: wrong OutputCount")
			return false
		}
	}

	addressInfo, err := liquid.GetAddressInfo(ClaimParties[0].Address)
	if err != nil {
		return false
	}

	ok := false
	for _, output := range decodedNew.Outputs {
		// 50 sats maximum fee allowed
		if output.Script.Address == addressInfo.Unconfidential && liquid.ToBitcoin(ClaimParties[0].Amount)-output.Amount < 0.0000005 {
			ok = true
		}
	}

	if ok {
		claimPSET = newClaimPSET
		return true
	}

	log.Println("PSET verification failed: output address not found or insufficient amount")
	return false
}
