///////////////////////////////////////////////////////////////////////////////
// Copyright © 2020 xx network SEZC                                          //
//                                                                           //
// Use of this source code is governed by a license that can be found in the //
// LICENSE file                                                              //
///////////////////////////////////////////////////////////////////////////////

// Contains instance-related functionality, unrelated to messaging

package cmd

import (
	"encoding/base64"
	"fmt"
	"github.com/pkg/errors"
	jww "github.com/spf13/jwalterweatherman"
	"gitlab.com/elixxir/comms/gateway"
	pb "gitlab.com/elixxir/comms/mixmessages"
	"gitlab.com/elixxir/comms/network"
	ds "gitlab.com/elixxir/comms/network/dataStructures"
	"gitlab.com/elixxir/gateway/notifications"
	"gitlab.com/elixxir/gateway/storage"
	"gitlab.com/elixxir/primitives/knownRounds"
	"gitlab.com/elixxir/primitives/states"
	"gitlab.com/xx_network/comms/connect"
	"gitlab.com/xx_network/comms/gossip"
	"gitlab.com/xx_network/primitives/id"
	"gitlab.com/xx_network/primitives/ndf"
	"gitlab.com/xx_network/primitives/rateLimiting"
	"gitlab.com/xx_network/primitives/utils"
	"gorm.io/gorm"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Errors to suppress
const (
	ErrInvalidHost = "Invalid host ID:"
	ErrAuth        = "Failed to authenticate id:"
	gwChanLen      = 1000
	period         = int64(1800000000000) // 30 minutes in nanoseconds
)

// The max number of rounds to be stored in the KnownRounds buffer.
const knownRoundsSize = 65536

type Instance struct {
	// Storage buffer for messages to be submitted to the network
	UnmixedBuffer storage.UnmixedMessageBuffer

	// Contains all Gateway relevant fields
	Params Params

	// Contains Server Host Information
	ServerHost *connect.Host

	// Gateway object created at start
	Comms *gateway.Comms

	// Map of leaky buckets for user IDs
	rateLimitQuit chan struct{}
	rateLimit     *rateLimiting.BucketMap

	// struct for tracking notifications
	un notifications.UserNotifications

	// Tracker of the gateway's known rounds
	knownRound *knownRounds.KnownRounds

	storage *storage.Storage
	// TODO: Integrate and remove duplication with the stuff above.
	// NetInf is the network interface for working with the NDF poll
	// functionality in comms.
	NetInf        *network.Instance
	addGateway    chan network.NodeGateway
	removeGateway chan *id.ID

	lastUpdate  uint64
	period      int64   // Defines length of validity for ClientBloomFilter
	lowestRound *uint64 // Cache lowest known BloomFilter round for client retrieval

	bloomFilterGossip sync.Mutex
}

// NewGatewayInstance initializes a gateway Handler interface
func NewGatewayInstance(params Params) *Instance {
	newDatabase, err := storage.NewStorage(params.DbUsername,
		params.DbPassword,
		params.DbName,
		params.DbAddress,
		params.DbPort,
		params.DevMode,
	)
	if err != nil {
		eMsg := fmt.Sprintf("Could not initialize database: "+
			"psql://%s@%s:%s/%s", params.DbUsername,
			params.DbAddress, params.DbPort, params.DbName)
		if params.DevMode {
			jww.WARN.Printf(eMsg)
		} else {
			jww.FATAL.Panicf(eMsg)
		}
	}
	i := &Instance{
		UnmixedBuffer: storage.NewUnmixedMessagesMap(),
		Params:        params,
		storage:       newDatabase,
		knownRound:    knownRounds.NewKnownRound(knownRoundsSize),
	}

	// There is no round 0
	i.knownRound.Check(0)
	jww.DEBUG.Printf("Initial KnownRound State: %+v", i.knownRound)
	msh, _ := i.knownRound.Marshal()
	jww.DEBUG.Printf("Initial KnownRound Marshal: %s",
		string(msh))

	return i
}

func NewImplementation(instance *Instance) *gateway.Implementation {
	impl := gateway.NewImplementation()
	impl.Functions.ConfirmNonce = func(message *pb.RequestRegistrationConfirmation) (confirmation *pb.RegistrationConfirmation, e error) {
		return instance.ConfirmNonce(message)
	}
	impl.Functions.PutMessage = func(message *pb.GatewaySlot) (*pb.GatewaySlotResponse, error) {
		return instance.PutMessage(message)
	}
	impl.Functions.RequestNonce = func(message *pb.NonceRequest) (nonce *pb.Nonce, e error) {
		return instance.RequestNonce(message)
	}
	impl.Functions.PollForNotifications = func(auth *connect.Auth) (i []*id.ID, e error) {
		return instance.PollForNotifications(auth)
	}
	// Client -> Gateway historical round request
	impl.Functions.RequestHistoricalRounds = func(msg *pb.HistoricalRounds) (response *pb.HistoricalRoundsResponse, err error) {
		return instance.RequestHistoricalRounds(msg)
	}
	// Client -> Gateway message request
	impl.Functions.RequestMessages = func(msg *pb.GetMessages) (*pb.GetMessagesResponse, error) {
		return instance.RequestMessages(msg)
	}
	impl.Functions.Poll = func(msg *pb.GatewayPoll) (response *pb.GatewayPollResponse, err error) {
		return instance.Poll(msg)
	}

	impl.Functions.ShareMessages = func(msg *pb.RoundMessages, auth *connect.Auth) error {
		return instance.ShareMessages(msg, auth)
	}
	return impl
}

// CreateNetworkInstance will generate a new network instance object given
// properly formed ndf, partialNdf, connection, and Storage object
func CreateNetworkInstance(conn *gateway.Comms, ndf, partialNdf *pb.NDF, ers *storage.Storage) (
	*network.Instance, error) {
	newNdf := &ds.Ndf{}
	newPartialNdf := &ds.Ndf{}
	err := newNdf.Update(ndf)
	if err != nil {
		return nil, err
	}
	err = newPartialNdf.Update(partialNdf)
	if err != nil {
		return nil, err
	}
	pc := conn.ProtoComms
	return network.NewInstance(pc, newNdf.Get(), newPartialNdf.Get(), ers, network.None)
}

// Start sets up the threads and network server to run the gateway
func (gw *Instance) Start() {
	// Now that we're set up, run a thread that constantly
	// polls for updates
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		for range ticker.C {
			msg, err := PollServer(gw.Comms,
				gw.ServerHost,
				gw.NetInf.GetFullNdf(),
				gw.NetInf.GetPartialNdf(),
				gw.lastUpdate,
				gw.Params.PublicAddress)
			if err != nil {
				jww.WARN.Printf(
					"Failed to Poll: %v",
					err)
				continue
			}
			err = gw.UpdateInstance(msg)
			if err != nil {
				jww.WARN.Printf("Unable to update instance: %+v", err)
			}
		}
	}()
}

// UpdateInstance reads a ServerPollResponse object and updates the instance
// state accordingly.
func (gw *Instance) UpdateInstance(newInfo *pb.ServerPollResponse) error {
	// Update the NDFs, and update the round info, which is currently
	// recorded but not used for anything. (maybe we should print state
	// of each round?)
	if newInfo.FullNDF != nil {
		err := gw.NetInf.UpdateFullNdf(newInfo.FullNDF)
		if err != nil {
			return err
		}
	}
	if newInfo.PartialNDF != nil {
		err := gw.NetInf.UpdatePartialNdf(newInfo.PartialNDF)
		if err != nil {
			return err
		}
	}

	if err := gw.NetInf.UpdateGatewayConnections(); err != nil {
		jww.ERROR.Printf("Failed to update gateway connections: %+v",
			err)
	}

	if newInfo.Updates != nil {

		for _, update := range newInfo.Updates {
			if update.UpdateID > gw.lastUpdate {
				gw.lastUpdate = update.UpdateID

				// Save lastUpdate ID to file
				if err := gw.SaveLastUpdateID(); err != nil {
					jww.ERROR.Print(err)
				}
			}
			// Parse the topology into an id list
			idList, err := id.NewIDListFromBytes(update.Topology)
			if err != nil {
				return err
			}

			err = gw.NetInf.RoundUpdate(update)
			if err != nil {
				// do not return on round update failure, that will cause the
				// gateway to cease to process further updates, just warn
				jww.WARN.Printf("failed to insert round update for %d: %s", update.ID, err)
			}

			// Convert the ID list to a circuit
			topology := ds.NewCircuit(idList)

			// Chek if our node is the entry point fo the circuit
			if states.Round(update.State) == states.PRECOMPUTING &&
				topology.IsFirstNode(gw.ServerHost.GetId()) {
				gw.UnmixedBuffer.SetAsRoundLeader(id.Round(update.ID), update.BatchSize)
			}
		}

		// get the earliest update and set the earliest known round to
		// it if the earliest known round is zero (meaning we dont have one)
		earliestRound := gw.NetInf.GetOldestRoundID()
		atomic.CompareAndSwapUint64(gw.lowestRound, 0,
			uint64(earliestRound))

	}

	// Send a new batch to the server when it asks for one
	if newInfo.BatchRequest != nil {
		gw.SendBatch(newInfo.BatchRequest)
	}
	// Process a batch that has been completed by this server
	if newInfo.Batch != nil {
		gw.ProcessCompletedBatch(newInfo.Batch.Slots, id.Round(newInfo.Batch.RoundID))
	}

	return nil
}

// sprintRoundInfo prints the interesting parts of the round info object.
func sprintRoundInfo(ri *pb.RoundInfo) string {
	roundStates := []string{"NOT_STARTED", "Waiting", "Precomp", "Standby",
		"Realtime", "Completed", "Error", "Crash"}
	topology := "v"
	for i := 0; i < len(ri.Topology); i++ {
		topology += "->" + base64.StdEncoding.EncodeToString(
			ri.Topology[i])
	}
	riStr := fmt.Sprintf("ID: %d, UpdateID: %d, State: %s, BatchSize: %d,"+
		"Topology: %s, RQTimeout: %d, Errors: %v",
		ri.ID, ri.UpdateID, roundStates[ri.State], ri.BatchSize, topology,
		ri.ResourceQueueTimeoutMillis, ri.Errors)
	return riStr
}

// InitNetwork initializes the network on this gateway instance
// After the network object is created, you need to use it to connect
// to the corresponding server in the network using ConnectToNode.
// Additionally, to clean up the network object (especially in tests), call
// Shutdown() on the network object.
func (gw *Instance) InitNetwork() error {
	var err error
	var gwCert, gwKey, nodeCert, permissioningCert []byte

	// Read our cert from file
	gwCert, err = utils.ReadFile(gw.Params.CertPath)
	if err != nil {
		return errors.Errorf("Failed to read certificate at %s: %+v",
			gw.Params.CertPath, err)
	}

	// Read our private key from file
	gwKey, err = utils.ReadFile(gw.Params.KeyPath)
	if err != nil {
		return errors.Errorf("Failed to read gwKey at %s: %+v",
			gw.Params.KeyPath, err)
	}

	// Read our node's cert from file
	nodeCert, err = utils.ReadFile(gw.Params.ServerCertPath)
	if err != nil {
		return errors.Errorf("Failed to read server gwCert at %s: %+v",
			gw.Params.ServerCertPath, err)
	}

	// Read the permissioning server's cert from
	permissioningCert, err = utils.ReadFile(gw.Params.PermissioningCertPath)
	if err != nil {
		return errors.WithMessagef(err,
			"Failed to read permissioning cert at %v",
			gw.Params.PermissioningCertPath)
	}

	// Load knownRounds data from storage if it exists
	if err := gw.LoadKnownRounds(); err != nil {
		jww.WARN.Printf("Unable to load KnownRounds: %+v", err)
	}

	// Load lastUpdate ID from storage if it exists
	if err := gw.LoadLastUpdateID(); err != nil {
		jww.WARN.Printf("Unable to load LastUpdateID: %+v", err)
	}

	// Set up temporary gateway listener
	gatewayHandler := NewImplementation(gw)
	gw.Comms = gateway.StartGateway(&id.TempGateway, gw.Params.ListeningAddress,
		gatewayHandler, gwCert, gwKey, gossip.DefaultManagerFlags())

	// Set gw.lowestRound information
	zeroRound := uint64(0)
	gw.lowestRound = &zeroRound

	// Set up temporary server host
	// (id, address string, cert []byte, disableTimeout, enableAuth bool)
	dummyServerID := id.DummyUser.DeepCopy()
	dummyServerID.SetType(id.Node)
	params := connect.GetDefaultHostParams()
	params.MaxRetries = 0
	gw.ServerHost, err = connect.NewHost(dummyServerID, gw.Params.NodeAddress,
		nodeCert, params)
	if err != nil {
		return errors.Errorf("Unable to create tmp server host: %+v",
			err)
	}

	// Get permissioning address from server
	permissioningAddr, err := gw.Comms.SendGetPermissioningAddress(gw.ServerHost)
	if err != nil {
		return errors.Errorf("Failed to get permissioning address from "+
			"server: %+v", err)
	}

	// Add permissioning host
	permissioningParams := connect.GetDefaultHostParams()
	permissioningParams.MaxRetries = 0
	permissioningParams.AuthEnabled = false
	_, err = gw.Comms.AddHost(&id.Permissioning, permissioningAddr,
		permissioningCert, permissioningParams)
	if err != nil {
		return errors.Errorf("Failed to add permissioning host: %+v", err)
	}

	// Begin polling server for NDF
	jww.INFO.Printf("Beginning polling NDF...")
	var nodeId []byte
	var serverResponse *pb.ServerPollResponse

	// fixme: determine if this a proper conditional for when server is not ready
	for serverResponse == nil {
		// TODO: Probably not great to always sleep immediately
		time.Sleep(3 * time.Second)

		// Poll Server for the NDFs, then use it to create the
		// network instance and begin polling for server updates
		serverResponse, err = PollServer(gw.Comms, gw.ServerHost, nil, nil, 0,
			gw.Params.PublicAddress)
		if err != nil {
			eMsg := err.Error()
			// Catch recoverable error
			if strings.Contains(eMsg, ErrInvalidHost) {
				jww.WARN.Printf("Node not ready...: %s",
					eMsg)
				continue
				// NO_NDF will be returned if the node
				// has not retrieved an NDF from
				// permissioning yet
			} else if strings.Contains(eMsg, ndf.NO_NDF) {
				continue
			} else if strings.Contains(eMsg, ErrAuth) {
				jww.WARN.Printf(eMsg)
				continue
			} else {
				return errors.Errorf(
					"Error polling NDF: %+v", err)
			}
		}

		// Install the NDF once we get it
		if serverResponse.FullNDF != nil && serverResponse.Id != nil {
			netDef, err := ndf.Unmarshal(serverResponse.FullNDF.Ndf)
			if err != nil {
				jww.WARN.Printf("failed to unmarshal the ndf: %+v", err)
				return err
			}
			err = gw.setupIDF(serverResponse.Id, netDef)
			nodeId = serverResponse.Id
			if err != nil {
				jww.WARN.Printf("failed to update node information: %+v", err)
				return err
			}
		}
		jww.INFO.Printf("Successfully obtained NDF!")

		// Replace the comms server with the newly-signed certificate
		// fixme: determine if we need to restart gw for restart with new id
		gw.Comms.Shutdown()

		serverID, err2 := id.Unmarshal(nodeId)
		if err2 != nil {
			jww.ERROR.Printf("Unmarshalling serverID failed during network "+
				"init: %+v", err2)
		}
		gw.ServerHost.Disconnect()

		// Update the host information with the new server ID
		params = connect.GetDefaultHostParams()
		params.MaxRetries = 0
		gw.ServerHost, err = connect.NewHost(serverID.DeepCopy(), gw.Params.NodeAddress, nodeCert,
			params)
		if err != nil {
			return errors.Errorf(
				"Unable to create updated server host: %+v", err)
		}

		gatewayId := serverID
		gatewayId.SetType(id.Gateway)
		gw.Comms = gateway.StartGateway(gatewayId, gw.Params.ListeningAddress,
			gatewayHandler, gwCert, gwKey, gossip.DefaultManagerFlags())

		jww.INFO.Printf("Creating instance!")
		gw.NetInf, err = CreateNetworkInstance(gw.Comms,
			serverResponse.FullNDF,
			serverResponse.PartialNDF, gw.storage)
		if err != nil {
			jww.ERROR.Printf("Unable to create network"+
				" instance: %v", err)
			continue
		}

		// Add permissioning as a host
		params := connect.GetDefaultHostParams()
		params.MaxRetries = 0
		params.AuthEnabled = false
		_, err = gw.Comms.AddHost(&id.Permissioning, permissioningAddr,
			permissioningCert, params)
		if err != nil {
			return errors.Errorf("Couldn't add permissioning host to comms: %v", err)
		}

		gw.addGateway = make(chan network.NodeGateway, gwChanLen)
		gw.removeGateway = make(chan *id.ID, gwChanLen)
		gw.NetInf.SetAddGatewayChan(gw.addGateway)
		gw.NetInf.SetRemoveGatewayChan(gw.removeGateway)

		// Enable authentication on gateway to gateway communications
		gw.NetInf.SetGatewayAuthentication()

		// Turn on gossiping
		if !gw.Params.DisableGossip {
			//gw.InitRateLimitGossip()
			gw.InitBloomGossip()
		}

		// Update the network instance
		// This must be below the enabling of the gossip above because it uses
		// components they initialize
		jww.INFO.Printf("Updating instance")
		err = gw.UpdateInstance(serverResponse)
		if err != nil {
			jww.ERROR.Printf("Update instance error: %v", err)
			continue
		}

		// newNdf := gw.NetInf.GetPartialNdf().Get()

		// Add notification bot as a host
		// _, err = gw.Comms.AddHost(&id.NotificationBot, newNdf.Notification.Address,
		// 	[]byte(newNdf.Notification.TlsCertificate), false, true)
		// if err != nil {
		// 	return errors.Errorf("Unable to add notifications host: %+v", err)
		// }
	}

	// Start storage cleanup thread
	go func() {
		gw.beginStorageCleanup()
	}()

	return nil
}

// Async function for cleaning up gateway storage
// and managing variables that need updated after cleanup
func (gw *Instance) beginStorageCleanup() {

	earliestRound, err := gw.storage.GetLowestBloomRound()
	if err != nil {
		jww.WARN.Printf("Unable to GetLowestBloomRound, will use the"+
			" lowest round on the first poll: %+v", err)
	}
	atomic.StoreUint64(gw.lowestRound, earliestRound)

	time.Sleep(1 * time.Second)

	// Begin ticker for storage cleanup
	ticker := time.NewTicker(gw.Params.cleanupInterval)
	retentionPeriod := gw.Params.retentionPeriod
	for true {
		select {
		case <-ticker.C:
			// Run storage cleanup when timer expires
			err := gw.clearOldStorage(time.Now().Add(-retentionPeriod))
			if err != nil {
				jww.WARN.Printf("Issue clearing old storage: %v", err)
				continue
			}
			// Update lowestRound information after cleanup
			earliestRound, err = gw.storage.GetLowestBloomRound()
			if err != nil {
				jww.WARN.Printf("Unable to GetLowestBloomRound: %+v", err)
				continue
			}
			atomic.StoreUint64(gw.lowestRound, earliestRound)
		}
	}
}

// Clears out old messages, rounds and bloom filters
func (gw *Instance) clearOldStorage(threshold time.Time) error {
	// Clear out old rounds and messages
	err := gw.storage.ClearOldStorage(threshold)
	if err != nil {
		return errors.Errorf("Could not clear old rounds and/or messages: %v", err)
	}

	// Clear out filters by epoch
	timestamp := time.Unix(0, threshold.UnixNano()).UnixNano()
	epoch := GetEpoch(timestamp, gw.period)
	err = gw.storage.DeleteClientFiltersBeforeEpoch(epoch)
	if err != nil {
		return errors.Errorf("Could not clear bloom filters: %v", err)
	}

	return nil
}

// Set the gw.period attribute
// NOTE: Saves the constant to storage if it does not exist
//       or reads an existing value from storage and sets accordingly
//       It's not great but it's structured this way as a business requirement
func (gw *Instance) SetPeriod() error {
	// Get an existing Period value from storage
	periodStr, err := gw.storage.GetStateValue(storage.PeriodKey)
	if err != nil &&
		!strings.Contains(err.Error(), gorm.ErrRecordNotFound.Error()) &&
		!strings.Contains(err.Error(), "Unable to locate state for key") {
		// If the error is unrelated to record not in storage, return it
		return err
	}

	if len(periodStr) > 0 {
		// If period already stored, use that value
		gw.period, err = strconv.ParseInt(periodStr, 10, 64)
	} else {
		// If period not already stored, use periodConst
		gw.period = period
		err = gw.storage.UpsertState(&storage.State{
			Key:   storage.PeriodKey,
			Value: strconv.FormatInt(period, 10),
		})
	}
	return err
}

// SaveKnownRounds saves the KnownRounds to a file.
func (gw *Instance) SaveKnownRounds() error {
	// Serialize knownRounds
	data, err := gw.knownRound.Marshal()
	if err != nil {
		return errors.Errorf("Failed to marshal KnownRounds: %v", err)
	}

	// Store knownRounds data
	return gw.storage.UpsertState(&storage.State{
		Key:   storage.KnownRoundsKey,
		Value: string(data),
	})

}

// LoadKnownRounds loads the KnownRounds from storage into the Instance, if a
// stored value exists.
func (gw *Instance) LoadKnownRounds() error {

	// Get an existing knownRounds value from storage
	data, err := gw.storage.GetStateValue(storage.KnownRoundsKey)
	if err != nil {
		return err
	}

	// Parse the data and store in the instance
	err = gw.knownRound.Unmarshal([]byte(data))
	if err != nil {
		return errors.Errorf("Failed to unmarshal KnownRounds: %v", err)
	}

	return nil
}

// SaveLastUpdateID saves the Instance.lastUpdate value to storage
func (gw *Instance) SaveLastUpdateID() error {
	data := strconv.FormatUint(gw.lastUpdate, 10)

	return gw.storage.UpsertState(&storage.State{
		Key:   storage.LastUpdateKey,
		Value: data,
	})

}

// LoadLastUpdateID loads the Instance.lastUpdate from storage into the Instance,
// if the key exists.
func (gw *Instance) LoadLastUpdateID() error {
	// Get an existing lastUpdate value from storage
	data, err := gw.storage.GetStateValue(storage.LastUpdateKey)
	if err != nil {
		return err
	}

	// Parse the last update
	dataStr := strings.TrimSpace(data)
	lastUpdate, err := strconv.ParseUint(dataStr, 10, 64)
	if err != nil {
		return errors.Errorf("Failed to get LastUpdateID: %v", err)
	}

	gw.lastUpdate = lastUpdate
	return nil
}
