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
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"net"
	"strings"
	"time"

	eh "github.com/firawe/eventhorizon"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
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

// EventStore implements an EventStore for MongoDB.
type EventStore struct {
	snapshotStore eh.SnapshotStore
	session       *mgo.Session
}

type Options struct {
	SSL        bool
	DBHost     string
	DBName     string
	DBUser     string
	DBPassword string
}

// NewEventStore creates a new EventStore.
func NewEventStore(options Options) (*EventStore, error) {
	session, err := initDB(options)
	if err != nil {
		return nil, ErrCouldNotDialDB
	}

	session.SetMode(mgo.Strong, true)
	session.SetSafe(&mgo.Safe{W: 1})

	return NewEventStoreWithSession(session)
}

// InitDB inits the database
func initDB(options Options) (*mgo.Session, error) {
	dialInfo := &mgo.DialInfo{
		Addrs:    strings.Split(options.DBHost, ","),
		Database: options.DBName,
		Username: options.DBUser,
		Password: options.DBPassword,
		DialServer: func(addr *mgo.ServerAddr) (net.Conn, error) {
			return tls.Dial("tcp", addr.String(), &tls.Config{InsecureSkipVerify: true})
		},
		ReplicaSetName: "rs0",
		Timeout:        time.Second * 10,
	}

	if !options.SSL {
		dialInfo.ReplicaSetName = ""
		dialInfo.DialServer = nil
	}
	// connect to the database
	session, err := mgo.DialWithInfo(dialInfo)
	if err != nil {
		return nil, err
	}
	return session, err
}

// NewEventStoreWithSession creates a new EventStore with a session.
func NewEventStoreWithSession(session *mgo.Session) (*EventStore, error) {
	if session == nil {
		return nil, ErrNoDBSession
	}

	s := &EventStore{
		session: session,
	}

	return s, nil
}

// Save implements the Save method of the eventhorizon.EventStore interface.
func (s *EventStore) Save(ctx context.Context, events []eh.Event, originalVersion int) error {
	if len(events) == 0 {
		return eh.EventStoreError{
			Err:           eh.ErrNoEventsToAppend,
			Namespace:     eh.NamespaceFromContext(ctx),
			AggregateType: eh.AggregateTypeFromContext(ctx),
		}
	}

	sess := s.session.Copy()
	defer sess.Close()

	// Build all event records, with incrementing versions starting from the
	// original aggregate version.
	dbEvents := make([]dbEvent, len(events))
	aggregateID := events[0].AggregateID()
	version := originalVersion
	for i, event := range events {
		// Only accept events belonging to the same aggregate.
		if event.AggregateID() != aggregateID {
			return eh.EventStoreError{
				Err:           eh.ErrInvalidEvent,
				Namespace:     eh.NamespaceFromContext(ctx),
				AggregateType: eh.AggregateTypeFromContext(ctx),
			}
		}

		// Only accept events that apply to the correct aggregate version.
		if event.Version() != version+1 {
			return eh.EventStoreError{
				Err:           eh.ErrIncorrectEventVersion,
				Namespace:     eh.NamespaceFromContext(ctx),
				AggregateType: eh.AggregateTypeFromContext(ctx),
			}
		}

		// Create the event record for the DB.
		e, err := newDBEvent(ctx, event)
		if err != nil {
			return err
		}
		if len(e.ID) == 0 {
			e.ID = uuid.New().String()
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
		if dbEvents[0].ID == "" {
			dbEvents[0].ID = uuid.New().String()
		}

		for i := range dbEvents {
			if dbEvents[i].ID == "" {
				dbEvents[i].ID = uuid.New().String()
			}
			_, err := sess.DB(s.dbName(ctx)).C(s.colName(ctx)+".events").Upsert(
				bson.M{
					"_id": dbEvents[i].ID,
				},
				bson.M{
					"$set": dbEvents[i],
				},
			)
			if err != nil {
				return eh.EventStoreError{
					BaseErr:       err,
					Err:           ErrCouldNotSaveAggregate,
					Namespace:     eh.NamespaceFromContext(ctx),
					AggregateType: eh.AggregateTypeFromContext(ctx),
				}
			}
		}

		if err := sess.DB(s.dbName(ctx)).C(s.colName(ctx)).Insert(aggregate); err != nil {
			return eh.EventStoreError{
				BaseErr:       err,
				Err:           ErrCouldNotSaveAggregate,
				Namespace:     eh.NamespaceFromContext(ctx),
				AggregateType: eh.AggregateTypeFromContext(ctx),
			}
		}
	} else {
		// Increment aggregate version on insert of new event record, and
		// only insert if version of aggregate is matching (ie not changed
		// since loading the aggregate).
		for i := range dbEvents {
			_, err := sess.DB(s.dbName(ctx)).C(s.colName(ctx)+".events").Upsert(
				bson.M{
					"_id": dbEvents[i].ID,
				},
				bson.M{
					"$set": dbEvents[i],
				},
			)
			if err != nil {
				return eh.EventStoreError{
					BaseErr:       err,
					Err:           ErrCouldNotSaveAggregate,
					Namespace:     eh.NamespaceFromContext(ctx),
					AggregateType: eh.AggregateTypeFromContext(ctx),
				}
			}
		}

		if err := sess.DB(s.dbName(ctx)).C(s.colName(ctx)).Update(
			bson.M{
				"_id":     aggregateID,
				"version": originalVersion,
			},
			bson.M{
				"$inc": bson.M{"version": len(dbEvents)},
			},
		); err != nil {
			return eh.EventStoreError{
				BaseErr:       err,
				Err:           ErrCouldNotSaveAggregate,
				Namespace:     eh.NamespaceFromContext(ctx),
				AggregateType: eh.AggregateTypeFromContext(ctx),
			}
		}
	}

	return nil
}

// Load implements the Load method of the eventhorizon.EventStore interface.
func (s *EventStore) Load(ctx context.Context, id string) ([]eh.Event, context.Context, error) {
	sess := s.session.Copy()
	defer sess.Close()

	batch := false
	var err error
	var minVersion int
	limit, ok := ctx.Value("limit").(int)
	if ok {
		batch = true
		minVersion, _ = ctx.Value("minVersion").(int)
	}
	//load dbEvents
	query := bson.M{
		"aggregate_id": id,
		"version":      bson.M{"$gte": minVersion},
	}
	var result []dbEvent
	if batch {
		err = sess.DB(s.dbName(ctx)).C(s.colName(ctx) + ".events").Find(query).Sort("version").Limit(limit).All(&result)
	} else {
		err = sess.DB(s.dbName(ctx)).C(s.colName(ctx) + ".events").Find(query).Sort("version").All(&result)
	}

	if err == mgo.ErrNotFound {
		return []eh.Event{}, ctx, nil
	} else if err != nil {
		return nil, ctx, eh.EventStoreError{
			BaseErr:   err,
			Err:       err,
			Namespace: eh.NamespaceFromContext(ctx),
		}
	}
	events := make([]eh.Event, len(result))

	for i, dbEvent := range result {
		// Create an event of the correct type.
		if data, err := eh.CreateEventData(dbEvent.EventType); err == nil {
			// Manually decode the raw BSON event.
			if err := dbEvent.RawData.Unmarshal(data); err != nil {
				return nil, ctx, eh.EventStoreError{
					BaseErr:   err,
					Err:       ErrCouldNotUnmarshalEvent,
					Namespace: eh.NamespaceFromContext(ctx),
				}
			}

			// Set conrcete event and zero out the decoded event.
			dbEvent.data = data
			dbEvent.RawData = bson.Raw{}
		}

		events[i] = event{dbEvent: dbEvent}
	}

	return events, ctx, nil
}

// Replace implements the Replace method of the eventhorizon.EventStore interface.
func (s *EventStore) Replace(ctx context.Context, event eh.Event) error {
	sess := s.session.Copy()
	defer sess.Close()

	// First check if the aggregate exists, the not found error in the update
	// query can mean both that the aggregate or the event is not found.
	n, err := sess.DB(s.dbName(ctx)).C(s.colName(ctx)).FindId(event.AggregateID()).Count()
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

	err = sess.DB(s.dbName(ctx)).C(s.colName(ctx)+".events").Update(
		bson.M{
			"aggregate_id": e.AggregateID,
			"version":      e.Version,
		},
		bson.M{
			"$set": bson.M{
				"data":       e.RawData,
				"timestamp":  e.Timestamp,
				"event_type": e.EventType,
			},
		},
	)
	if err == mgo.ErrNotFound {
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

// RenameEvent implements the RenameEvent method of the eventhorizon.EventStore interface.
func (s *EventStore) RenameEvent(ctx context.Context, from, to eh.EventType) error {
	sess := s.session.Copy()
	defer sess.Close()

	// Find and rename all events.
	// TODO: Maybe use change info.
	if _, err := sess.DB(s.dbName(ctx)).C(s.colName(ctx)+".events").UpdateAll(
		bson.M{
			"event_type": string(from),
		},
		bson.M{
			"$set": bson.M{"event_type": string(to)},
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

// Clear clears the event storage.
func (s *EventStore) Clear(ctx context.Context) error {
	if err := s.session.DB(s.dbName(ctx)).C(s.colName(ctx)).DropCollection(); err != nil {
		return eh.EventStoreError{
			BaseErr:       err,
			Err:           ErrCouldNotClearDB,
			Namespace:     eh.NamespaceFromContext(ctx),
			AggregateType: eh.AggregateTypeFromContext(ctx),
		}
	}
	if err := s.session.DB(s.dbName(ctx)).C(s.colName(ctx) + ".events").DropCollection(); err != nil {
		return eh.EventStoreError{
			BaseErr:       err,
			Err:           ErrCouldNotClearDB,
			Namespace:     eh.NamespaceFromContext(ctx),
			AggregateType: eh.AggregateTypeFromContext(ctx),
		}
	}
	return nil
}

// Close closes the database session.
func (s *EventStore) Close() {
	s.session.Close()
}

// DBName appends the namespace, if one is set, to the DB prefix to
// get the name of the DB to use.
func (s *EventStore) dbName(ctx context.Context) string {
	return eh.NamespaceFromContext(ctx)
}

func (s *EventStore) colName(ctx context.Context) string {
	return eh.AggregateTypeFromContext(ctx)
}

// aggregateRecord is the DB representation of an aggregate.
type aggregateRecord struct {
	AggregateID string    `bson:"_id"`
	Version     int       `bson:"version"`
	Events      []dbEvent `bson:"-"`
	// Type        string        `bson:"type"`
	// Snapshot    bson.Raw      `bson:"snapshot"`
}

// dbEvent is the internal event record for the MongoDB event store used
// to save and load events from the DB.
type dbEvent struct {
	ID            string           `bson:"_id"`
	AggregateType eh.AggregateType `bson:"aggregate_type"`
	AggregateID   string           `bson:"aggregate_id"`
	EventType     eh.EventType     `bson:"event_type"`
	RawData       bson.Raw         `bson:"data,omitempty"`
	data          eh.EventData     `bson:"-"`
	Timestamp     time.Time        `bson:"timestamp"`
	Version       int              `bson:"version"`
}

// newDBEvent returns a new dbEvent for an event.
func newDBEvent(ctx context.Context, event eh.Event) (*dbEvent, error) {
	// Marshal event data if there is any.
	var rawData bson.Raw
	if event.Data() != nil {
		raw, err := bson.Marshal(event.Data())
		if err != nil {
			return nil, eh.EventStoreError{
				BaseErr:       err,
				Err:           ErrCouldNotMarshalEvent,
				Namespace:     eh.NamespaceFromContext(ctx),
				AggregateType: eh.AggregateTypeFromContext(ctx),
			}
		}
		rawData = bson.Raw{Kind: 3, Data: raw}
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

func (e event) ID() string {
	return e.dbEvent.ID
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
