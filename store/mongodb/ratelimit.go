package mongostore

import (
	"context"
	"errors"
	"time"

	"github.com/faultvane/faultvane-go/ratelimit"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var errRateLimitContention = errors.New("too many concurrent changes of one rate limit bucket")

type RateLimitStore struct {
	store *Store
}

type rateLimitDocument struct {
	Version int64     `bson:"version"`
	Tokens  float64   `bson:"tokens"`
	At      time.Time `bson:"at"`
	Now     time.Time `bson:"now"`
}

func NewRateLimitStore(collection *mongo.Collection, opts ...Option) (*RateLimitStore, error) {
	store, err := New(collection, opts...)
	if err != nil {
		return nil, err
	}
	return &RateLimitStore{store: store}, nil
}

func (s *RateLimitStore) id(name string) string {
	return s.store.id(name) + ":rl:bucket"
}

func (s *RateLimitStore) EnsureIndexes(ctx context.Context) error {
	return s.store.EnsureIndexes(ctx)
}

func exceptDuplicateKey(err error) error {
	if mongo.IsDuplicateKeyError(err) {
		return nil
	}
	return err
}

func (s *RateLimitStore) attempt(ctx context.Context, name string, request ratelimit.Acquire) (ratelimit.Grant, bool, error) {
	var doc rateLimitDocument
	prepare := mongo.Pipeline{{{Key: "$set", Value: bson.D{
		{Key: "now", Value: "$$NOW"},
		{Key: "version", Value: bson.D{{Key: "$ifNull", Value: bson.A{"$version", 0}}}},
		{Key: "tokens", Value: bson.D{{Key: "$ifNull", Value: bson.A{"$tokens", float64(request.Burst)}}}},
		{Key: "at", Value: bson.D{{Key: "$ifNull", Value: bson.A{"$at", "$$NOW"}}}},
		{Key: "expireAt", Value: bson.D{{Key: "$ifNull", Value: bson.A{"$expireAt", bson.D{{Key: "$add", Value: bson.A{"$$NOW", request.StateTTL.Milliseconds()}}}}}}},
	}}}}
	err := s.store.collection.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: s.id(name)}}, prepare,
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)).Decode(&doc)
	if err != nil {
		return ratelimit.Grant{}, false, exceptDuplicateKey(err)
	}
	bucket, grant := ratelimit.Bucket{Tokens: doc.Tokens, At: doc.At}.Acquire(doc.Now, request)
	result, err := s.store.collection.UpdateOne(ctx,
		bson.D{{Key: "_id", Value: s.id(name)}, {Key: "version", Value: doc.Version}},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "tokens", Value: bucket.Tokens},
			{Key: "at", Value: bucket.At},
			{Key: "version", Value: doc.Version + 1},
			{Key: "expireAt", Value: bucket.At.Add(request.StateTTL)},
		}}})
	if err != nil {
		return ratelimit.Grant{}, false, err
	}
	return grant, result.MatchedCount == 1, nil
}

func (s *RateLimitStore) Acquire(ctx context.Context, name string, request ratelimit.Acquire) (ratelimit.Grant, error) {
	for range maxAttempts {
		grant, done, err := s.attempt(ctx, name, request)
		if err != nil {
			return ratelimit.Grant{}, s.store.fail("rate limit acquire", err)
		}
		if done {
			return grant, nil
		}
	}
	return ratelimit.Grant{}, s.store.fail("rate limit acquire", errRateLimitContention)
}
