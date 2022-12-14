///////////////////////////////////////////////////////////////////////////////
// Copyright © 2020 xx network SEZC                                          //
//                                                                           //
// Use of this source code is governed by a license that can be found in the //
// LICENSE file                                                              //
///////////////////////////////////////////////////////////////////////////////

package cmd

import (
	"bytes"
	"encoding/binary"
	"time"

	"github.com/pkg/errors"
	jww "github.com/spf13/jwalterweatherman"
	pb "gitlab.com/elixxir/comms/mixmessages"
	"gitlab.com/elixxir/comms/network"
	"gitlab.com/elixxir/crypto/cmix"
	"gitlab.com/elixxir/crypto/cyclic"
	"gitlab.com/elixxir/crypto/hash"
	"gitlab.com/elixxir/gateway/storage"
	"gitlab.com/elixxir/primitives/format"
	"gitlab.com/elixxir/primitives/states"
	"gitlab.com/xx_network/primitives/id"
	"gitlab.com/xx_network/primitives/id/ephemeral"
)

// Zeroed identity fingerprint identifies dummy messages
var dummyIdFp = make([]byte, format.IdentityFPLen)

// Determines the Epoch value of the given timestamp with the given period
func GetEpoch(ts int64, period int64) uint32 {
	if period == 0 {
		jww.FATAL.Panicf("GetEpoch: Divide by zero")
	} else if ts < 0 || period < 0 {
		jww.FATAL.Panicf("GetEpoch: Negative input")
	}
	return uint32(ts / period)
}

// Determines the timestamp value of the given epoch
func GetEpochTimestamp(epoch uint32, period int64) int64 {
	return period * int64(epoch)
}

// Client -> Gateway handler. Looks up messages based on a userID and a roundID.
// If the gateway participated in this round, and the requested client had messages in that round,
// we return these message(s) to the requester
func (gw *Instance) RequestMessages(req *pb.GetMessages) (*pb.GetMessagesResponse, error) {
	// Error check for an invalidly crafted message
	if req == nil || req.ClientID == nil || req.RoundID == 0 {
		return &pb.GetMessagesResponse{}, errors.New("Could not parse message! " +
			"Please try again with a properly crafted message!")
	}

	// Parse the requested clientID within the message for the database request
	userId, err := ephemeral.Marshal(req.ClientID)
	if err != nil {
		return &pb.GetMessagesResponse{}, errors.Errorf("Could not parse requested user ID: %+v", err)
	}

	// Parse the roundID within the message
	roundID := id.Round(req.RoundID)

	// Search the database for the requested messages
	msgs, isValidGateway, err := gw.storage.GetMixedMessages(userId, roundID)
	if err != nil {
		jww.WARN.Printf("Could not find any MixedMessages with "+
			"recipient ID %v and round ID %v: %+v", userId, roundID, err)
		return &pb.GetMessagesResponse{
				HasRound: true,
			}, errors.Errorf("Could not find any MixedMessages with "+
				"recipient ID %v and round ID %v: %+v", userId, roundID, err)
	} else if !isValidGateway {
		jww.WARN.Printf("A client (%v) has requested messages for a "+
			"round (%v) which is not recorded with messages", userId, roundID)
		return &pb.GetMessagesResponse{
			HasRound: false,
		}, nil
	}

	// Parse the database response to construct individual slots
	var slots []*pb.Slot
	for _, msg := range msgs {
		// Get the message contents
		payloadA, payloadB := msg.GetMessageContents()
		// Construct the slot and place in the list
		data := &pb.Slot{
			PayloadA: payloadA,
			PayloadB: payloadB,
		}
		jww.DEBUG.Printf("Message Retrieved for: %d", userId.Int64())

		slots = append(slots, data)
	}

	// Return all messages to the requester
	return &pb.GetMessagesResponse{
		HasRound: true,
		Messages: slots,
	}, nil

}

// RequestHistoricalRounds retrieves all rounds requested within the HistoricalRounds
// message from the gateway's database. A list of round info messages are returned
// to the sender
func (gw *Instance) RequestHistoricalRounds(msg *pb.HistoricalRounds) (*pb.HistoricalRoundsResponse, error) {
	// Nil check external messages to avoid potential crashes
	if msg == nil || msg.Rounds == nil {
		return &pb.HistoricalRoundsResponse{}, errors.New("Invalid historical" +
			" round request, could not look up rounds. Please send a valid message.")
	}

	// Parse the message for all requested rounds
	var roundIds []id.Round
	for _, rnd := range msg.Rounds {
		roundIds = append(roundIds, id.Round(rnd))
	}
	// Look up requested rounds in the database
	retrievedRounds, err := gw.storage.RetrieveMany(roundIds)
	if err != nil {
		return &pb.HistoricalRoundsResponse{}, errors.New("Could not look up rounds requested.")
	}

	// Return the retrievedRounds
	return &pb.HistoricalRoundsResponse{
		Rounds: retrievedRounds,
	}, nil

}

// PutMessage adds a message to the outgoing queue
func (gw *Instance) PutMessage(msg *pb.GatewaySlot) (*pb.GatewaySlotResponse, error) {
	// Construct Client ID for database lookup
	clientID, err := id.Unmarshal(msg.Message.SenderID)
	if err != nil {
		return &pb.GatewaySlotResponse{
			Accepted: false,
		}, errors.Errorf("Could not parse message: Unrecognized ID")
	}

	// Retrieve the client from the database
	cl, err := gw.storage.GetClient(clientID)
	if err != nil {
		return &pb.GatewaySlotResponse{
			Accepted: false,
		}, errors.New("Did not recognize ID. Have you registered successfully?")
	}

	// Generate the MAC and check against the message's MAC
	clientMac := generateClientMac(cl, msg)
	if !bytes.Equal(clientMac, msg.MAC) {
		return &pb.GatewaySlotResponse{
			Accepted: false,
		}, errors.New("Could not authenticate client. Is the client registered with this node?")
	}
	thisRound := id.Round(msg.RoundID)

	// Rate limit messages
	senderId, err := id.Unmarshal(msg.GetMessage().GetSenderID())
	if err != nil {
		return nil, errors.Errorf("Unable to unmarshal sender ID: %+v", err)
	}

	/*if !gw.Params.DisableGossip {
		err = gw.FilterMessage(senderId)
		if err != nil {
			jww.INFO.Printf("Rate limiting check failed on send message from "+
				"%v", msg.Message.GetSenderID())
			return &pb.GatewaySlotResponse{
				Accepted: false,
			}, err
		}
	}*/

	if err = gw.UnmixedBuffer.AddUnmixedMessage(msg.Message, thisRound); err != nil {
		return &pb.GatewaySlotResponse{Accepted: false},
			errors.WithMessage(err, "could not add to round. "+
				"Please try a different round.")
	}

	if jww.GetLogThreshold() <= jww.LevelDebug {
		msgFmt := format.NewMessage(gw.NetInf.GetCmixGroup().GetP().ByteLen())
		msgFmt.SetPayloadA(msg.Message.PayloadA)
		msgFmt.SetPayloadB(msg.Message.PayloadB)
		jww.DEBUG.Printf("Putting message from user %s (msgDigest: %s) "+
			"in outgoing queue for round %d...", senderId.String(),
			msgFmt.Digest(), thisRound)
	}

	return &pb.GatewaySlotResponse{
		Accepted: true,
		RoundID:  msg.GetRoundID(),
	}, nil
}

// Helper function which generates the client MAC for checking the clients
// authenticity
func generateClientMac(cl *storage.Client, msg *pb.GatewaySlot) []byte {
	// Digest the message for the MAC generation
	gatewaySlotDigest := network.GenerateSlotDigest(msg)

	// Hash the clientGatewayKey and then the slot's salt
	h, _ := hash.NewCMixHash()
	h.Write(cl.Key)
	h.Write(msg.Message.Salt)
	hashed := h.Sum(nil)

	h.Reset()

	// Hash the gatewaySlotDigest and the above hashed data
	h.Write(hashed)
	h.Write(gatewaySlotDigest)

	return h.Sum(nil)
}

// Pass-through for Registration Nonce Communication
func (gw *Instance) RequestNonce(msg *pb.NonceRequest) (*pb.Nonce, error) {
	jww.INFO.Print("Passing on registration nonce request")
	return gw.Comms.SendRequestNonceMessage(gw.ServerHost, msg)

}

// Pass-through for Registration Nonce Confirmation
func (gw *Instance) ConfirmNonce(msg *pb.RequestRegistrationConfirmation) (*pb.RegistrationConfirmation, error) {

	jww.INFO.Print("Passing on registration nonce confirmation")

	resp, err := gw.Comms.SendConfirmNonceMessage(gw.ServerHost, msg)

	if err != nil {
		return resp, err
	}

	// Insert client information to database
	newClient := &storage.Client{
		Id:  msg.UserID,
		Key: resp.ClientGatewayKey,
	}

	err = gw.storage.UpsertClient(newClient)
	if err != nil {
		return resp, nil
	}

	return resp, nil
}

// GenJunkMsg generates a junk message using the gateway's client key
func GenJunkMsg(grp *cyclic.Group, numNodes int, msgNum uint32, roundID id.Round) *pb.Slot {

	baseKey := grp.NewIntFromBytes(id.DummyUser[:])

	var baseKeys []*cyclic.Int

	for i := 0; i < numNodes; i++ {
		baseKeys = append(baseKeys, baseKey)
	}

	salt := make([]byte, 32)
	salt[0] = 0x01

	msg := format.NewMessage(grp.GetP().ByteLen())
	payloadBytes := make([]byte, grp.GetP().ByteLen())
	bs := make([]byte, 4)
	// Note: Cannot be 0, must be inside group
	// So we add 1, and start at offset in payload
	// to avoid both conditions
	binary.LittleEndian.PutUint32(bs, msgNum+1)
	for i := 0; i < len(bs); i++ {
		payloadBytes[i+1] = bs[i]
	}
	msg.SetPayloadA(payloadBytes)
	msg.SetPayloadB(payloadBytes)

	ephId, _, _, err := ephemeral.GetId(&id.DummyUser, 64, time.Now().UnixNano())
	if err != nil {
		jww.FATAL.Panicf("Could not get ID: %+v", err)
	}
	msg.SetEphemeralRID(ephId[:])
	msg.SetIdentityFP(dummyIdFp)

	ecrMsg := cmix.ClientEncrypt(grp, msg, salt, baseKeys, roundID)

	h, err := hash.NewCMixHash()
	if err != nil {
		jww.FATAL.Printf("Could not get hash: %+v", err)
	}

	KMACs := cmix.GenerateKMACs(salt, baseKeys, roundID, h)
	return &pb.Slot{
		PayloadB: ecrMsg.GetPayloadB(),
		PayloadA: ecrMsg.GetPayloadA(),
		Salt:     salt,
		SenderID: id.DummyUser.Marshal(),
		KMACs:    KMACs,
	}
}

// SendBatch polls sends whatever messages are in the batch associated with the
// requested round to the server
func (gw *Instance) SendBatch(roundInfo *pb.RoundInfo) {

	batchSize := uint64(roundInfo.BatchSize)
	if batchSize == 0 {
		jww.WARN.Printf("Server sent empty roundBufferSize!")
		return
	}

	rid := id.Round(roundInfo.ID)

	batch := gw.UnmixedBuffer.PopRound(rid)

	if batch == nil {
		jww.ERROR.Printf("Batch for %v not found!", roundInfo.ID)
		return
	}

	batch.Round = roundInfo

	jww.INFO.Printf("Sending batch for round %d with %d messages...",
		roundInfo.ID, len(batch.Slots))

	numNodes := len(roundInfo.GetTopology())

	if numNodes == 0 {
		jww.ERROR.Println("Round topology empty, sending bad messages!")
	}

	// Now fill with junk and send
	for i := uint64(len(batch.Slots)); i < batchSize; i++ {
		junkMsg := GenJunkMsg(gw.NetInf.GetCmixGroup(), numNodes,
			uint32(i), rid)
		batch.Slots = append(batch.Slots, junkMsg)
	}

	// Send the completed batch
	err := gw.Comms.PostNewBatch(gw.ServerHost, batch)
	if err != nil {
		// TODO: handle failure sending batch
		jww.WARN.Printf("Error while sending batch: %v", err)
	}

	if !gw.Params.DisableGossip {
		/*// Gossip senders included in the batch to other gateways
		err = gw.GossipBatch(batch)
		if err != nil {
			jww.WARN.Printf("Unable to gossip batch information: %+v", err)
		}*/
	}
}

// ProcessCompletedBatch handles messages coming out of the mixnet
func (gw *Instance) ProcessCompletedBatch(msgs []*pb.Slot, roundID id.Round) {
	if len(msgs) == 0 {
		return
	}

	// At this point, the returned batch and its fields should be non-nil
	round, err := gw.NetInf.GetRound(roundID)
	if err != nil {
		jww.ERROR.Printf("ProcessCompleted - Unable to get "+
			"round %d: %+v", roundID, err)
		return
	}

	recipients, clientRound := gw.processMessages(msgs, roundID, round)

	//upsert messages to the database
	go func() {
		errMsg := gw.storage.InsertMixedMessages(clientRound)
		if errMsg != nil {
			jww.ERROR.Printf("Inserting new mixed messages failed in "+
				"ProcessCompletedBatch for round %d: %+v", roundID, errMsg)
		}
	}()
	jww.INFO.Printf("Shairing Messages with teamates for round %d", roundID)
	// Share messages in the batch with the rest of the team
	err = gw.sendShareMessages(msgs, round)
	if err != nil {
		// Print error but do not stop message processing
		jww.ERROR.Printf("Message sharing failed for "+
			"round %d: %+v", roundID, err)
	}

	// Gossip recipients included in the completed batch to other gateways
	// in a new thread
	if !gw.Params.DisableGossip {
		jww.INFO.Printf("Sending bloom gossip (source thread) for round %d", roundID)
		go func() {
			jww.INFO.Printf("Sending bloom gossip (new thread) for round %d", roundID)
			errGossip := gw.GossipBloom(recipients, roundID, int64(round.Timestamps[states.QUEUED]))
			if err != nil {
				jww.ERROR.Printf("Unable to gossip bloom information "+
					"for round %d: %+v", roundID, errGossip)
			}
			jww.INFO.Printf("Sent bloom gossip for round %d", roundID)
		}()

		go func() {
			// Update filters in our storage system
			errFilters := gw.UpsertFilters(recipients, roundID)
			if err != nil {
				jww.ERROR.Printf("Unable to update local bloom filters "+
					"for round %d: %+v", roundID, errFilters)
			}
		}()
	}

	go PrintProfilingStatistics()
}

// Helper function which takes passed in messages from a round and
// stores these as mixedMessages
func (gw *Instance) processMessages(msgs []*pb.Slot, roundID id.Round,
	round *pb.RoundInfo) (map[ephemeral.Id]interface{}, *storage.ClientRound) {
	numReal := 0

	// Build a ClientRound object around the client messages
	clientRound := &storage.ClientRound{
		Id:        uint64(roundID),
		Timestamp: time.Unix(0, int64(round.Timestamps[states.QUEUED])),
	}
	msgsToInsert := make([]storage.MixedMessage, len(msgs))
	recipients := make(map[ephemeral.Id]interface{})
	// Process the messages into the ClientRound object
	for _, msg := range msgs {
		serialMsg := format.NewMessage(gw.NetInf.GetCmixGroup().GetP().ByteLen())
		serialMsg.SetPayloadA(msg.GetPayloadA())
		serialMsg.SetPayloadB(msg.GetPayloadB())

		// If IdentityFP is not zeroed, the message is not a dummy
		if !bytes.Equal(serialMsg.GetIdentityFP(), dummyIdFp) {
			recipIdBytes := serialMsg.GetEphemeralRID()
			recipientId, err := ephemeral.Marshal(recipIdBytes)
			if err != nil {
				jww.ERROR.Printf("Unable to marshal ID: %+v", err)
				continue
			}

			// Clear random bytes from recipient ID and add to map
			recipientId = recipientId.Clear(uint(round.AddressSpaceSize))
			if recipientId.Int64() != 0 {
				recipients[recipientId] = nil
			}

			if jww.GetStdoutThreshold() <= jww.LevelDebug {
				msgFmt := format.NewMessage(gw.NetInf.GetCmixGroup().GetP().ByteLen())
				msgFmt.SetPayloadA(msg.PayloadA)
				msgFmt.SetPayloadB(msg.PayloadB)

				jww.DEBUG.Printf("Message received for: %d[%d] in "+
					"round: %d, msgDigest: %s", recipientId.Int64(),
					round.AddressSpaceSize, roundID, msgFmt.Digest())
			}

			// Create new message and add it to the list for insertion
			msgsToInsert[numReal] = *storage.NewMixedMessage(roundID, recipientId, msg.PayloadA, msg.PayloadB)
			numReal++
		}
	}

	// Perform the message insertion into Storage
	clientRound.Messages = msgsToInsert[:numReal]

	jww.INFO.Printf("Round %d received, %d real messages "+
		"processed, %d dummies ignored", clientRound.Id, numReal,
		len(msgs)-numReal)

	return recipients, clientRound
}

// FilterMessage determines if the message should be kept or discarded based on
// the capacity of the sender's ID bucket.
func (gw *Instance) FilterMessage(userId *id.ID) error {
	// If the user ID bucket is full AND the message's user ID is not on the
	// whitelist, then reject the message
	if !gw.rateLimit.LookupBucket(userId.String()).Add(1) {
		return errors.New("Rate limit exceeded. Try again later.")
	}

	// Otherwise, if the user ID bucket has room then let the message through
	return nil
}
