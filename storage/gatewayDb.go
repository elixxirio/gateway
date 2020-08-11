////////////////////////////////////////////////////////////////////////////////
// Copyright © 2020 Privategrity Corporation                                   /
//                                                                             /
// All rights reserved.                                                        /
////////////////////////////////////////////////////////////////////////////////

// Handles the database ORM for gateways

package storage

import (
	"github.com/jinzhu/gorm"
	"gitlab.com/xx_network/primitives/id"
)

// Returns a Client from Storage with the given id
// Or an error if a matching Client does not exist
func (d *DatabaseImpl) GetClient(id *id.ID) (*Client, error) {
	result := &Client{}
	err := d.db.First(&result, "id = ?", id.Marshal()).Error
	return result, err
}

// Inserts the given Client into Storage
// Returns an error if a Client with a matching Id already exists
func (d *DatabaseImpl) InsertClient(client *Client) error {
	return d.db.Create(client).Error
}

// Returns a Round from Storage with the given id
// Or an error if a matching Round does not exist
func (d *DatabaseImpl) GetRound(id *id.Round) (*Round, error) {
	result := &Round{}
	err := d.db.First(&result, "id = ?", uint64(*id)).Error
	return result, err
}

// Returns multiple Rounds from Storage with the given ids
// Or an error if no matching Rounds exist
func (d *DatabaseImpl) GetRounds(ids []*id.Round) ([]*Round, error) {
	// Convert IDs to plain numbers
	plainIds := make([]uint64, len(ids))
	for i, v := range ids {
		plainIds[i] = uint64(*v)
	}

	// Execute the query
	results := make([]*Round, 0)
	err := d.db.Where("id IN (?)", plainIds).Find(&results).Error

	return results, err
}

// Inserts the given Round into Storage if it does not exist
// Or updates the given Round if the provided Round UpdateId is greater
func (d *DatabaseImpl) UpsertRound(round *Round) error {
	// Make a copy of the provided round
	newRound := *round

	// Build a transaction to prevent race conditions
	return d.db.Transaction(func(tx *gorm.DB) error {

		// Attempt to insert the round into the database,
		// or if it already exists, replace round with the database value
		err := tx.FirstOrCreate(round, &Round{Id: round.Id}).Error
		if err != nil {
			return err
		}

		// If the provided round has a greater UpdateId than the database value,
		// overwrite the database value with the provided round
		if round.UpdateId < newRound.UpdateId {
			return tx.Save(&newRound).Error
		}

		// Commit
		return nil
	})
}

// Returns a slice of MixedMessages from Storage
// with matching recipientId and roundId
// Or an error if a matching Round does not exist
func (d *DatabaseImpl) GetMixedMessages(recipientId *id.ID, roundId *id.Round) ([]*MixedMessage, error) {
	results := make([]*MixedMessage, 0)
	err := d.db.Find(&results,
		&MixedMessage{RecipientId: recipientId.Marshal(),
			RoundId: uint64(*roundId)}).Error
	return results, err
}

// Inserts the given MixedMessage into Storage
// NOTE: Do not specify Id attribute, it is autogenerated
func (d *DatabaseImpl) InsertMixedMessage(msg *MixedMessage) error {
	return d.db.Create(msg).Error
}

// Deletes a MixedMessage with the given id from Storage
// Returns an error if a matching MixedMessage does not exist
func (d *DatabaseImpl) DeleteMixedMessage(id uint64) error {
	return d.db.Delete(&MixedMessage{
		Id: id,
	}).Error
}

// Returns a BloomFilter from Storage with the given clientId
// Or an error if a matching BloomFilter does not exist
func (d *DatabaseImpl) GetBloomFilters(clientId *id.ID) ([]*BloomFilter, error) {
	results := make([]*BloomFilter, 0)
	err := d.db.Find(&results,
		&BloomFilter{ClientId: clientId.Marshal()}).Error
	return results, err
}

// Inserts the given BloomFilter into Storage
// NOTE: Do not specify Id attribute, it is autogenerated
func (d *DatabaseImpl) InsertBloomFilter(filter *BloomFilter) error {
	return d.db.Create(filter).Error
}

// Deletes a BloomFilter with the given id from Storage
// Returns an error if a matching BloomFilter does not exist
func (d *DatabaseImpl) DeleteBloomFilter(id uint64) error {
	return d.db.Delete(&BloomFilter{
		Id: id,
	}).Error
}

// Returns a EphemeralBloomFilter from Storage with the given recipientId
// Or an error if a matching EphemeralBloomFilter does not exist
func (d *DatabaseImpl) GetEphemeralBloomFilters(recipientId *id.ID) ([]*EphemeralBloomFilter, error) {
	results := make([]*EphemeralBloomFilter, 0)
	err := d.db.Find(&results,
		&EphemeralBloomFilter{RecipientId: recipientId.Marshal()}).Error
	return results, err
}

// Inserts the given EphemeralBloomFilter into Storage
// NOTE: Do not specify Id attribute, it is autogenerated
func (d *DatabaseImpl) InsertEphemeralBloomFilter(filter *EphemeralBloomFilter) error {
	return d.db.Create(filter).Error
}

// Deletes a EphemeralBloomFilter with the given id from Storage
// Returns an error if a matching EphemeralBloomFilter does not exist
func (d *DatabaseImpl) DeleteEphemeralBloomFilter(id uint64) error {
	return d.db.Delete(&EphemeralBloomFilter{
		Id: id,
	}).Error
}
