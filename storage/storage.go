////////////////////////////////////////////////////////////////////////////////
// Copyright © 2020 Privategrity Corporation                                   /
//                                                                             /
// All rights reserved.                                                        /
////////////////////////////////////////////////////////////////////////////////

// Handles the high level storage API.
// This layer merges the business logic layer and the database layer

package storage

import (
	jww "github.com/spf13/jwalterweatherman"
	"gitlab.com/xx_network/primitives/id"
	"gitlab.com/xx_network/primitives/id/ephemeral"
)

// API for the storage layer
type Storage struct {
	// Stored database interface
	database
}

// Create a new Storage object wrapping a database interface
// Returns a Storage object, close function, and error
func NewStorage(username, password, dbName, address, port string) (*Storage, func() error, error) {
	db, closeFunc, err := newDatabase(username, password, dbName, address, port)
	storage := &Storage{db}
	return storage, closeFunc, err
}

// Builds a ClientBloomFilter with the given parameters, then stores it
func (s *Storage) HandleBloomFilter(recipientId *ephemeral.Id, filterBytes []byte, roundId id.Round, epoch uint32) error {

	// Build a newly-initialized ClientBloomFilter to be stored
	validFilter := &ClientBloomFilter{
		RecipientId: recipientId.Int64(),
		Epoch:       epoch,
		FirstRound:  uint64(roundId),
		RoundRange:  0,
		Filter:      filterBytes,
	}

	// Commit the new/updated ClientBloomFilter
	return s.upsertClientBloomFilter(validFilter)
}

// Returns a slice of MixedMessage from database with matching recipientId and roundId
// Also returns a boolean for whether the gateway contains other messages for the given Round
func (s *Storage) GetMixedMessages(recipientId *id.ID, roundId id.Round) (msgs []*MixedMessage, isValidGateway bool, err error) {
	// Determine whether this gateway has any messages for the given roundId
	count, err := s.countMixedMessagesByRound(roundId)
	isValidGateway = count > 0
	if err != nil || !isValidGateway {
		return
	}

	// If the gateway has messages, return messages relevant to the given recipientId and roundId
	msgs, err = s.getMixedMessages(recipientId, roundId)
	return
}

// Helper function for HandleBloomFilter
// Returns the bitwise OR of two byte slices
// TODO: Test
func or(l1, l2 []byte) []byte {
	if l1 == nil {
		return l2
	} else if l2 == nil {
		return l1
	} else if len(l1) != len(l2) {
		jww.ERROR.Printf("Unable to perform bitwise OR: Slice lens invalid.")
		return l1
	}

	result := make([]byte, len(l1))
	for i := range l1 {
		result[i] = l1[i] | l2[i]
	}
	return result
}

// Combine with and update this filter using oldFilter
// TODO: Test
func (f *ClientBloomFilter) combine(oldFilter *ClientBloomFilter) {
	// Initialize FirstRound variable if needed
	if oldFilter.FirstRound == uint64(0) {
		oldFilter.FirstRound = f.FirstRound
	}

	// Store variables before modifications
	oldLastRound := oldFilter.FirstRound + uint64(oldFilter.RoundRange)
	newLastRound := f.FirstRound + uint64(f.RoundRange)

	// Get earliest FirstRound Value
	if f.FirstRound > oldFilter.FirstRound {
		f.FirstRound = oldFilter.FirstRound
	}

	// Get latest LastRound value, and calculate the maximum RoundRange
	if oldLastRound > newLastRound {
		f.RoundRange = uint32(oldLastRound - f.FirstRound)
	} else {
		f.RoundRange = uint32(newLastRound - f.FirstRound)
	}

	// Combine the filters
	f.Filter = or(f.Filter, oldFilter.Filter)
}
