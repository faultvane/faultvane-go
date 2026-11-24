package mongostore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/circuitbreaker"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	maxPartLength = 64
	maxAttempts   = 16
	restoreTTL    = time.Hour
)

var errContention = errors.New("too many concurrent changes of one breaker")

type Store struct {
	collection *mongo.Collection
	namespace  string
}

type Option func(*Store)

func WithNamespace(namespace string) Option {
	return func(s *Store) { s.namespace = namespace }
}

type document struct {
	Version int64     `bson:"version"`
	Record  string    `bson:"record"`
	Now     time.Time `bson:"now"`
}

func validPart(part string) bool {
	if part == "" || len(part) > maxPartLength {
		return false
	}
	for _, r := range part {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

func New(collection *mongo.Collection, opts ...Option) (*Store, error) {
	s := &Store{collection: collection}
	for _, opt := range opts {
		opt(s)
	}
	switch {
	case collection == nil:
		return nil, fmt.Errorf("%w: collection must not be nil", faultvane.ErrInvalidConfig)
	case !validPart(s.namespace):
		return nil, fmt.Errorf("%w: Namespace must have 1 to 64 characters from a-z, A-Z, 0-9, '.', '_', '-'", faultvane.ErrInvalidConfig)
	}
	return s, nil
}

func (s *Store) id(name string) string {
	return s.namespace + ":" + name
}

func (s *Store) fail(operation string, err error) error {
	return fmt.Errorf("mongostore: %s: %w: %w", operation, faultvane.ErrStoreUnavailable, err)
}

func (s *Store) EnsureIndexes(ctx context.Context) error {
	_, err := s.collection.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "expireAt", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(0).SetName("faultvane_expire"),
	})
	if err != nil {
		return s.fail("ensure indexes", err)
	}
	return nil
}

func decode(text string) (circuitbreaker.Record, error) {
	var record circuitbreaker.Record
	err := json.Unmarshal([]byte(text), &record)
	return record, err
}

func (s *Store) read(ctx context.Context, name string, ttl time.Duration) (document, error) {
	var doc document
	prepare := mongo.Pipeline{{{Key: "$set", Value: bson.D{
		{Key: "now", Value: "$$NOW"},
		{Key: "version", Value: bson.D{{Key: "$ifNull", Value: bson.A{"$version", 0}}}},
		{Key: "record", Value: bson.D{{Key: "$ifNull", Value: bson.A{"$record", "{}"}}}},
		{Key: "expireAt", Value: bson.D{{Key: "$ifNull", Value: bson.A{"$expireAt", bson.D{{Key: "$add", Value: bson.A{"$$NOW", ttl.Milliseconds()}}}}}}},
	}}}}
	err := s.collection.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: s.id(name)}}, prepare,
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)).Decode(&doc)
	return doc, err
}

func (s *Store) attempt(ctx context.Context, name string, ttl time.Duration, apply func(circuitbreaker.Record, time.Time) circuitbreaker.Record) (circuitbreaker.Snapshot, bool, error) {
	doc, err := s.read(ctx, name, ttl)
	if mongo.IsDuplicateKeyError(err) {
		return circuitbreaker.Snapshot{}, false, nil
	}
	if err != nil {
		return circuitbreaker.Snapshot{}, false, err
	}
	record, err := decode(doc.Record)
	if err != nil {
		return circuitbreaker.Snapshot{}, false, err
	}
	record = apply(record, doc.Now)
	encoded, _ := json.Marshal(record)
	result, err := s.collection.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: s.id(name)}, {Key: "version", Value: doc.Version}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "record", Value: string(encoded)},
			{Key: "version", Value: doc.Version + 1},
			{Key: "expireAt", Value: doc.Now.Add(ttl)},
		}}})
	if err != nil {
		return circuitbreaker.Snapshot{}, false, err
	}
	return record.Snapshot(doc.Now), result.MatchedCount == 1, nil
}

func (s *Store) update(ctx context.Context, operation, name string, ttl time.Duration, apply func(circuitbreaker.Record, time.Time) circuitbreaker.Record) (circuitbreaker.Snapshot, error) {
	for range maxAttempts {
		snapshot, done, err := s.attempt(ctx, name, ttl, apply)
		if err != nil {
			return circuitbreaker.Snapshot{}, s.fail(operation, err)
		}
		if done {
			return snapshot, nil
		}
	}
	return circuitbreaker.Snapshot{}, s.fail(operation, errContention)
}

func (s *Store) serverTime(ctx context.Context) (time.Time, error) {
	var reply struct {
		LocalTime time.Time `bson:"localTime"`
	}
	err := s.collection.Database().RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&reply)
	return reply.LocalTime, err
}

func (s *Store) Load(ctx context.Context, name string) (circuitbreaker.Snapshot, error) {
	var doc document
	projection := bson.D{{Key: "record", Value: 1}, {Key: "now", Value: "$$NOW"}}
	err := s.collection.FindOne(ctx, bson.D{{Key: "_id", Value: s.id(name)}}, options.FindOne().SetProjection(projection)).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		doc.Record = "{}"
		doc.Now, err = s.serverTime(ctx)
	}
	var record circuitbreaker.Record
	if err == nil {
		record, err = decode(doc.Record)
	}
	if err != nil {
		return circuitbreaker.Snapshot{}, s.fail("load", err)
	}
	return record.Snapshot(doc.Now), nil
}

func (s *Store) Report(ctx context.Context, name string, report circuitbreaker.Report) (circuitbreaker.Snapshot, error) {
	return s.update(ctx, "report", name, report.StateTTL, func(r circuitbreaker.Record, now time.Time) circuitbreaker.Record {
		return r.Report(now, report)
	})
}

func (s *Store) AcquireProbe(ctx context.Context, name string, request circuitbreaker.ProbeRequest) (circuitbreaker.ProbeGrant, error) {
	granted := false
	snapshot, err := s.update(ctx, "acquire probe", name, request.StateTTL, func(r circuitbreaker.Record, now time.Time) circuitbreaker.Record {
		r, granted = r.AcquireProbe(now, request)
		return r
	})
	return circuitbreaker.ProbeGrant{Snapshot: snapshot, Granted: granted && err == nil}, err
}

func (s *Store) CompleteProbe(ctx context.Context, name string, result circuitbreaker.ProbeResult) (circuitbreaker.Snapshot, error) {
	return s.update(ctx, "complete probe", name, result.StateTTL, func(r circuitbreaker.Record, now time.Time) circuitbreaker.Record {
		return r.CompleteProbe(now, result)
	})
}

func (s *Store) Restore(ctx context.Context, name string, snapshot circuitbreaker.Snapshot) (circuitbreaker.Snapshot, error) {
	return s.update(ctx, "restore", name, restoreTTL, func(r circuitbreaker.Record, _ time.Time) circuitbreaker.Record {
		return r.Restore(snapshot)
	})
}
