//go:build !cln

package ln

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"time"

	"peerswap-web/cmd/psweb/bitcoin"
	"peerswap-web/cmd/psweb/config"
	"peerswap-web/cmd/psweb/db"
	"peerswap-web/cmd/psweb/ps"
)

var (
	myPrivateKey *rsa.PrivateKey
	myPublicPEM  string
	// maps learned public keys to node Id
	pemToNodeId map[string]string
	// public key of the sender of pegin_started broadcast
	PeginHandler string
	// when currently pending pegin can be claimed
	ClaimBlockHeight uint32
	// human readable status of the claimjoin
	ClaimStatus = "No third-party pegin is pending"
	// none, initiator or joiner
	MyRole = "none"
	// array of initiator + joiners, for initiator only
	claimParties []*ClaimParty
	// requires repeat of the last transmission
	sayAgain = false
)

type Coordination struct {
	// action required from the peer: "add", "remove", "process", "process2", "continue", "say_again"
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
	PEM         string
	LastMessage *Coordination
	SentTime    time.Time
}

/*
	// first pegin
	peginTxId := "0b387c3b7a8d9fad4d7a1ac2cba4958451c03d6c4fe63dfbe10cdb86d666cdd7"
	peginVout := uint(0)
	peginRawTx := "0200000000010159c1a062851325f301282b8ae1124c98228ea7b100eb82e907c6c314001a11860100000000fdffffff03a08601000000000017a914c74490c48b105376e697925c314c47afb00f1303872b1400000000000017a914b5e28484d1f1a5d74878f6ef411d555ac170e62887400d03000000000017a9146ec12f7b07420d693b59c8e6c20b17fbe40503ae87024730440220687049a9caf086d6a2205534acde99e549bde1bb922cfb6f7a7f5204a48ccebc02201c0d84dff34c3ce455fc6806015c160fa35eee86d2fddc7ceebdc3eb4d3d18d20121038d432d6aa857671ed9b7c67d62b0d2ae3c930e203024978c92a0de8b84142ce58a910000"
	peginTxoutProof := "0000002015fa046cecb94e68d91a1c9410d3a220c34a738121b56f17270000000000000036a1af20a2d5bd8c81ddd4b1444f14b28c9c3d049fb2ae6280b0b1ebf19aca64ca4caa660793601934cfc0800e000000044934a86fa650c138bd770a4ca085d35118f433f02f7660ac8b77d4e99120a241cf9ddc148344fd9ed151ecc9b73697ac6d7ac34a2e1ae588f0b39e5b26e39841cd0c149927b444b7cfbb1c9a14f6f7b1e7e578ba2fb55d3999df736fc087e89fd7cd66d686db0ce1fb3de64f6c3dc0518495a4cbc21a7a4dad9f8d7a3b7c380b01b5"
	peginClaimScript := "00142c72a18f520851c0da25c3b9a4a7be1daf65a7a3"
	peginAmount := uint64(100_000)
	liquidAddress := "el1qq2ssn76875d2624p8fmzlm4u959kasmuss0wl4hxdm6hrcz8syruxgx7esshkshs6rdxrrzru7ujw7ne6h3asd46hj3ruv8xh"

	// second pegin
	peginTxId2 := "0b387c3b7a8d9fad4d7a1ac2cba4958451c03d6c4fe63dfbe10cdb86d666cdd7"
	peginVout2 := uint(2)
	peginRawTx2 := "0200000000010159c1a062851325f301282b8ae1124c98228ea7b100eb82e907c6c314001a11860100000000fdffffff03a08601000000000017a914c74490c48b105376e697925c314c47afb00f1303872b1400000000000017a914b5e28484d1f1a5d74878f6ef411d555ac170e62887400d03000000000017a9146ec12f7b07420d693b59c8e6c20b17fbe40503ae87024730440220687049a9caf086d6a2205534acde99e549bde1bb922cfb6f7a7f5204a48ccebc02201c0d84dff34c3ce455fc6806015c160fa35eee86d2fddc7ceebdc3eb4d3d18d20121038d432d6aa857671ed9b7c67d62b0d2ae3c930e203024978c92a0de8b84142ce58a910000"
	peginTxoutProof2 := "0000002015fa046cecb94e68d91a1c9410d3a220c34a738121b56f17270000000000000036a1af20a2d5bd8c81ddd4b1444f14b28c9c3d049fb2ae6280b0b1ebf19aca64ca4caa660793601934cfc0800e000000044934a86fa650c138bd770a4ca085d35118f433f02f7660ac8b77d4e99120a241cf9ddc148344fd9ed151ecc9b73697ac6d7ac34a2e1ae588f0b39e5b26e39841cd0c149927b444b7cfbb1c9a14f6f7b1e7e578ba2fb55d3999df736fc087e89fd7cd66d686db0ce1fb3de64f6c3dc0518495a4cbc21a7a4dad9f8d7a3b7c380b01b5"
	peginClaimScript2 := "0014e6f7021314806b914a45cce95680b1377f0b7003"
	peginAmount2 := uint64(200_000)
	liquidAddress2 := "el1qqfun028g4f2nen6a5zj8t20jrsg258k023azkp075rx529g95nf2vysemv6qhkzlntx4gw3tn9ptc0ynr86nqvfaxkar73zzw"
	fee := uint64(33) // per pegin!

	psbt, err := liquid.CreateClaimPSBT(peginTxId,
		peginVout,
		peginRawTx,
		peginTxoutProof,
		peginClaimScript,
		peginAmount-fee,
		liquidAddress,
		peginTxId2,
		peginVout2,
		peginRawTx2,
		peginTxoutProof2,
		peginClaimScript2,
		peginAmount2-fee,
		liquidAddress2,
		fee*2)
	if err != nil {
		log.Fatalln(err)
	}

	blinded1, complete, err := liquid.ProcessPSBT(psbt, "swaplnd")
	if err != nil {
		log.Fatalln(err)
	}

	blinded2, complete, err := liquid.ProcessPSBT(blinded1, "swaplnd2")
	if err != nil {
		log.Fatalln(err)
	}

	signed1, complete, err := liquid.ProcessPSBT(blinded2, "swaplnd")
	if err != nil {
		log.Fatalln(err)
	}

	signed2, complete, err := liquid.ProcessPSBT(signed1, "swaplnd2")
	if err != nil {
		log.Fatalln(err)
	}

	hexTx, complete, err := liquid.FinalizePSBT(signed2)
	if err != nil {
		log.Fatalln(err)
	}
*/

// runs after restart, to continue if pegin is ongoing
func loadClaimJoinDB() {
	db.Load("ClaimJoin", "pemToNodeId", &pemToNodeId)
	db.Load("ClaimJoin", "PeginHandler", &PeginHandler)
	db.Load("ClaimJoin", "ClaimBlockHeight", &ClaimBlockHeight)
	db.Load("ClaimJoin", "ClaimStatus", &ClaimStatus)
	db.Load("ClaimJoin", "PrivateKey", &myPrivateKey)
	db.Load("ClaimJoin", "MyRole", &MyRole)
	db.Load("ClaimJoin", "claimParties", &claimParties)

	if MyRole == "joiner" && PeginHandler != "" {
		// ask to send again the last transmission
		sayAgain = true
	}
}

// runs every minute
func OnTimer() {

	//

	if sayAgain {
		if SendCoordination(PeginHandler, &Coordination{Action: "say_again"}) == nil {
			sayAgain = false
		}
	}
}

// Called when received a broadcast custom message
// Forward the message to all direct peers, unless the source PEM is known already
// (it means the message came back to you from a downstream peer)
func Broadcast(fromNodeId string, message *Message) error {

	if message.Asset == "pegin_started" && pemToNodeId[message.Sender] != "" || message.Asset == "pegin_ended" && pemToNodeId == nil {
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

	if message.Asset == "pegin_started" {
		// where to forward claimjoin request
		PeginHandler = message.Sender
		// store for relaying further encrypted messages
		pemToNodeId[message.Sender] = fromNodeId
		// currently expected ETA is communicated via Amount
		ClaimBlockHeight = uint32(message.Amount)
		ClaimStatus = "Pegin started, awaiting joiners"
	} else if message.Asset == "pegin_ended" {
		PeginHandler = ""
		// delete the routing map
		pemToNodeId = nil
		ClaimBlockHeight = 0
		ClaimStatus = "No third-party pegin is pending"
	}

	// persist to db
	db.Save("ClaimJoin", "pemToNodeId", pemToNodeId)
	db.Save("ClaimJoin", "PeginHandler", PeginHandler)
	db.Save("ClaimJoin", "ClaimBlockHeight", ClaimBlockHeight)
	db.Save("ClaimJoin", "ClaimStatus", ClaimStatus)

	return nil
}

// Encrypt and send message to an anonymous peer identified only by public key in PEM format
// New keys are generated at the start of each pegin session
// Peers track sources of encrypted messages to forward back the replies
func SendCoordination(destinationPEM string, message *Coordination) error {

	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}

	destinationNodeId := pemToNodeId[destinationPEM]
	if destinationNodeId == "" {
		log.Println("Cannot send coordination, destination PEM has no matching NodeId")
		return fmt.Errorf("destination PEM has no matching NodeId")
	}

	// Deserialize the received PEM string back to a public key
	deserializedPublicKey, err := pemToPublicKey(destinationPEM)
	if err != nil {
		log.Println("Error deserializing public key:", err)
		return err
	}

	// Encrypt the message using the deserialized receiver's public key
	ciphertext, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, deserializedPublicKey, payload, nil)
	if err != nil {
		log.Println("Error encrypting message:", err)
		return err
	}

	cl, clean, er := GetClient()
	if er != nil {
		return er
	}
	defer clean()

	return SendCustomMessage(cl, destinationNodeId, &Message{
		Version:     MessageVersion,
		Memo:        "process",
		Sender:      myPublicPEM,
		Destination: destinationPEM,
		Payload:     ciphertext,
	})
}

// Either forward to final distination or decrypt and process
func Process(message *Message) error {

	cl, clean, er := GetClient()
	if er != nil {
		return er
	}
	defer clean()

	if message.Destination == myPublicPEM {
		// Decrypt the message using my private key
		plaintext, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, myPrivateKey, message.Payload, nil)
		if err != nil {
			return fmt.Errorf("error decrypting message: %s", err)
		}

		// recover the struct
		var msg Coordination
		err = json.Unmarshal(plaintext, &msg)
		if err != nil {
			return fmt.Errorf("received an incorrectly formed message: %s", err)

		}

		log.Println(msg)

		if msg.ClaimBlockHeight > ClaimBlockHeight {
			ClaimBlockHeight = msg.ClaimBlockHeight
		}

		switch msg.Action {
		case "add":
			if MyRole != "initiator" {
				return fmt.Errorf("cannot add a joiner, not a claim initiator")
			}

		case "remove":
		case "process": // blind or sign
		case "process2": // process twice to blind and sign

		}

		return nil
	}

	// relay further
	destinationNodeId := pemToNodeId[message.Destination]
	if destinationNodeId == "" {
		return fmt.Errorf("cannot relay, destination PEM has no matching NodeId")
	}
	return SendCustomMessage(cl, destinationNodeId, message)

}

// generates new message keys
func GenerateKeys() error {
	// Generate RSA key pair
	var err error

	myPrivateKey, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Println("Error generating my private key:", err)
		return err
	}

	myPublicPEM, err = publicKeyToPEM(&myPrivateKey.PublicKey)
	if err != nil {
		log.Println("Error obtaining my public PEM:", err)
		return err
	}

	// persist to db
	db.Save("ClaimJoin", "PrivateKey", myPrivateKey)

	return nil
}

// Convert a public key to PEM format
func publicKeyToPEM(pub *rsa.PublicKey) (string, error) {
	pubASN1, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}

	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PUBLIC KEY",
		Bytes: pubASN1,
	})

	return string(pubPEM), nil
}

// Convert a PEM-formatted string to a public key
func pemToPublicKey(pubPEM string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pubPEM))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}

	switch pub := pub.(type) {
	case *rsa.PublicKey:
		return pub, nil
	default:
		return nil, fmt.Errorf("not an RSA public key")
	}
}

// called by claim join initiator after his pegin funding tx confirms
func InitiateClaimJoin(claimBlockHeight uint32) bool {
	if myNodeId == "" {
		// populates myNodeId
		if getLndVersion() == 0 {
			return false
		}
	}

	if Broadcast(myNodeId, &Message{
		Version: MessageVersion,
		Memo:    "broadcast",
		Asset:   "pegin_started",
		Amount:  uint64(claimBlockHeight),
	}) == nil {
		ClaimBlockHeight = claimBlockHeight
		party := createClaimParty(claimBlockHeight)
		if party != nil {
			// initiate array of claim parties
			claimParties = nil
			claimParties[0] = party
			// persist to db
			db.Save("ClaimJoin", "claimParties", claimParties)
			return true
		}
	}
	return false
}

// called by ClaimJoin joiner candidate after his pegin funding tx confirms
func JoinClaimJoin(claimBlockHeight uint32) bool {
	if myNodeId == "" {
		// populates myNodeId
		if getLndVersion() == 0 {
			return false
		}
	}

	party := createClaimParty(claimBlockHeight)
	if party != nil {
		if SendCoordination(PeginHandler, &Coordination{
			Action:           "add",
			Joiner:           *party,
			ClaimBlockHeight: claimBlockHeight,
		}) == nil {
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

	return party
}
