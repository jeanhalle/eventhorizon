// Copyright (c) 2015 - The Event Horizon authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mongodb

import (
	"context"
	"errors"
	"fmt"
	"time"

	eh "github.com/looplab/eventhorizon"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// ErrCouldNotDialDB is when the database could not be dialed.
var ErrCouldNotDialDB = errors.New("could not dial database")

// ErrNoDBSession is when no database session is set.
var ErrNoDBSession = errors.New("no database session")

// ErrCouldNotClearDB is when the database could not be cleared.
var ErrCouldNotClearDB = errors.New("could not clear database")

// ErrCouldNotMarshalEvent is when an event could not be marshaled into BSON.
var ErrCouldNotMarshalEvent = errors.New("could not marshal event")

// ErrCouldNotUnmarshalEvent is when an event could not be unmarshaled into a concrete type.
var ErrCouldNotUnmarshalEvent = errors.New("could not unmarshal event")

// ErrCouldNotLoadAggregate is when an aggregate could not be loaded.
var ErrCouldNotLoadAggregate = errors.New("could not load aggregate")

// ErrCouldNotSaveAggregate is when an aggregate could not be saved.
var ErrCouldNotSaveAggregate = errors.New("could not save aggregate")

// EventStore is a wrapper for the mongo-driver. Implements an EventStore for MongoDB
type EventStore struct {
	client   *mongo.Client
	dbPrefix string
}

//NewEventStore create a new Mongo Event Store
func NewEventStore(connectionString, dbPrefix string) (*EventStore, error) {

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	connectionString = fmt.Sprintf("mongodb://%s", connectionString)

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(connectionString))
	if err != nil {
		return nil, err
	}
	ctxping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = client.Ping(ctxping, readpref.Primary())
	if err != nil {
		return nil, err
	}

	return &EventStore{dbPrefix: dbPrefix, client: client}, nil
}

func (s *EventStore) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.client.Disconnect(ctx)
}

// Save implements the Save method of the eventhorizon.EventStore interface.
func (s *EventStore) Save(ctx context.Context, events []eh.Event, originalVersion int) error {
	if len(events) == 0 {
		return eh.EventStoreError{
			Err: eh.ErrNoEventsToAppend,
		}
	}

	// Build all event records, with incrementing versions starting from the
	// original aggregate version.
	dbEvents := make([]dbEvent, len(events))
	aggregateID := events[0].AggregateID()
	version := originalVersion
	for i, event := range events {
		// Only accept events belonging to the same aggregate.
		if event.AggregateID() != aggregateID {
			return eh.EventStoreError{
				Err: eh.ErrInvalidEvent,
			}
		}

		// Only accept events that apply to the correct aggregate version.
		if event.Version() != version+1 {
			return eh.EventStoreError{
				Err: eh.ErrIncorrectEventVersion,
			}
		}

		// Create the event record for the DB.
		e, err := newDBEvent(ctx, event)
		if err != nil {
			return err
		}
		dbEvents[i] = *e
		version++
	}

	// Either insert a new aggregate or append to an existing.
	if originalVersion == 0 {
		aggregate := aggregateRecord{
			AggregateID: aggregateID,
			Version:     len(dbEvents),
			Events:      dbEvents,
		}

		_, err := s.client.Database(s.dbName(ctx)).Collection("events").InsertOne(context.Background(), aggregate)
		if err != nil {
			return eh.EventStoreError{
				BaseErr: err,
				Err:     ErrCouldNotSaveAggregate,
			}
		}
	} else {
		// Increment aggregate version on insert of new event record, and
		// only insert if version of aggregate is matching (ie not changed
		// since loading the aggregate).
		filter := bson.D{
			{Key: "_id", Value: aggregateID},
			{Key: "version", Value: originalVersion},
		}

		if updateResult, err := s.client.Database(s.dbName(ctx)).Collection("events").UpdateOne(
			context.Background(), filter,
			bson.M{
				"$push": bson.M{"events": bson.M{"$each": dbEvents}},
				"$inc":  bson.M{"version": len(dbEvents)},
			},
		); err != nil || updateResult.MatchedCount != 1 {
			return eh.EventStoreError{
				BaseErr: err,
				Err:     ErrCouldNotSaveAggregate,
			}
		}
	}

	return nil
}

// Load implements the Load method of the eventhorizon.EventStore interface.
func (s *EventStore) Load(ctx context.Context, id string) ([]eh.Event, error) {
	var aggregate aggregateRecord
	filter := bson.D{{Key: "_id", Value: id}}
	err := s.client.Database(s.dbName(ctx)).Collection("events").FindOne(context.Background(), filter).Decode(&aggregate)
	if err == mongo.ErrNoDocuments {
		return []eh.Event{}, nil
	} else if err != nil {
		return nil, eh.EventStoreError{
			BaseErr: err,
			Err:     err,
		}
	}

	events := make([]eh.Event, len(aggregate.Events))
	for i, dbEvent := range aggregate.Events {
		// Create an event of the correct type.
		if data, err := eh.CreateEventData(dbEvent.EventType); err == nil {
			// Manually decode the raw BSON event.
			if err := bson.Unmarshal(dbEvent.RawData, data); err != nil {
				return nil, eh.EventStoreError{
					BaseErr: err,
					Err:     ErrCouldNotUnmarshalEvent,
				}
			}

			// Set conrcete event and zero out the decoded event.
			dbEvent.data = data
			dbEvent.RawData = bson.Raw{}
		}
		events[i] = event{dbEvent: dbEvent}
	}

	return events, nil
}

// Clear clears the event storage.
func (s *EventStore) Clear(ctx context.Context) error {

	if err := s.client.Database(s.dbName(ctx)).Collection("events").Drop(ctx); err != nil {
		return eh.EventStoreError{
			BaseErr:   err,
			Err:       ErrCouldNotClearDB,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}
	return nil
}

// RenameEvent implements the RenameEvent method of the eventhorizon.EventStore interface.
func (s *EventStore) RenameEvent(ctx context.Context, from, to eh.EventType) error {
	// Find and rename all events.
	// TODO: Maybe use change info.
	if _, err := s.client.Database(s.dbName(ctx)).Collection("events").UpdateMany(ctx,
		bson.M{
			"events.event_type": string(from),
		},
		bson.M{
			"$set": bson.M{"events.$.event_type": string(to)},
		},
	); err != nil {
		return eh.EventStoreError{
			BaseErr:   err,
			Err:       ErrCouldNotSaveAggregate,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	return nil
}

// Replace implements the Replace method of the eventhorizon.EventStore interface.
func (s *EventStore) Replace(ctx context.Context, event eh.Event) error {

	// First check if the aggregate exists, the not found error in the update
	// query can mean both that the aggregate or the event is not found.
	n, err := s.client.Database(s.dbName(ctx)).Collection("events").CountDocuments(ctx, bson.M{"_id": event.AggregateID()})
	if n == 0 {
		return eh.ErrAggregateNotFound
	} else if err != nil {
		return eh.EventStoreError{
			BaseErr:   err,
			Err:       err,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	// Create the event record for the DB.
	e, err := newDBEvent(ctx, event)
	if err != nil {
		return err
	}

	// Find and replace the event.
	result, err := s.client.Database(s.dbName(ctx)).Collection("events").UpdateOne(ctx,
		bson.M{
			"_id":            event.AggregateID(),
			"events.version": event.Version(),
		},
		bson.M{
			"$set": bson.M{"events.$": *e},
		},
	)
	if result.MatchedCount == 0 || err == mongo.ErrNoDocuments {
		return eh.ErrInvalidEvent
	} else if err != nil {
		return eh.EventStoreError{
			BaseErr:   err,
			Err:       ErrCouldNotSaveAggregate,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}

	return nil
}

// dbName appends the namespace, if one is set, to the DB prefix to
// get the name of the DB to use.
func (s *EventStore) dbName(ctx context.Context) string {
	ns := eh.NamespaceFromContext(ctx)
	return s.dbPrefix + "_" + ns
}

// aggregateRecord is the DB representation of an aggregate.
type aggregateRecord struct {
	AggregateID string    `bson:"_id"`
	Version     int       `bson:"version"`
	Events      []dbEvent `bson:"events"`
	// Type        string        `bson:"type"`
	// Snapshot    bson.Raw      `bson:"snapshot"`
}

// dbEvent is the internal event record for the MongoDB event store used
// to save and load events from the DB.
type dbEvent struct {
	EventType     eh.EventType     `bson:"event_type"`
	RawData       bson.Raw         `bson:"data,omitempty"`
	data          eh.EventData     `bson:"-"`
	Timestamp     time.Time        `bson:"timestamp"`
	AggregateType eh.AggregateType `bson:"aggregate_type"`
	AggregateID   string           `bson:"_id"`
	Version       int              `bson:"version"`
}

// newDBEvent returns a new dbEvent for an event.
func newDBEvent(ctx context.Context, event eh.Event) (*dbEvent, error) {
	// Marshal event data if there is any.
	var rawData bson.Raw
	if event.Data() != nil {
		raw, err := bson.Marshal(event.Data())
		if err != nil {
			// return nil, eh.EventStoreError{
			// 	BaseErr:   err,
			// 	Err:       ErrCouldNotMarshalEvent,
			// 	Namespace: eh.NamespaceFromContext(ctx),
			// }
		}
		rawData = raw
	}
	return &dbEvent{
		EventType:     event.EventType(),
		RawData:       rawData,
		Timestamp:     event.Timestamp(),
		AggregateType: event.AggregateType(),
		AggregateID:   event.AggregateID(),
		Version:       event.Version(),
	}, nil
}

// event is the private implementation of the eventhorizon.Event interface
// for a MongoDB event store.
type event struct {
	dbEvent
}

// AggrgateID implements the AggrgateID method of the eventhorizon.Event interface.
func (e event) AggregateID() string {
	return e.dbEvent.AggregateID
}

// AggregateType implements the AggregateType method of the eventhorizon.Event interface.
func (e event) AggregateType() eh.AggregateType {
	return e.dbEvent.AggregateType
}

// EventType implements the EventType method of the eventhorizon.Event interface.
func (e event) EventType() eh.EventType {
	return e.dbEvent.EventType
}

// Data implements the Data method of the eventhorizon.Event interface.
func (e event) Data() eh.EventData {
	return e.dbEvent.data
}

// Version implements the Version method of the eventhorizon.Event interface.
func (e event) Version() int {
	return e.dbEvent.Version
}

// Timestamp implements the Timestamp method of the eventhorizon.Event interface.
func (e event) Timestamp() time.Time {
	return e.dbEvent.Timestamp
}

// String implements the String method of the eventhorizon.Event interface.
func (e event) String() string {
	return fmt.Sprintf("%s@%d", e.dbEvent.EventType, e.dbEvent.Version)
}
