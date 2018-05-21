package redispub

import (
	"fmt"
	"time"

	"github.com/globalsign/mgo/bson"
	"github.com/go-redis/redis"
	"github.com/tulip/oplogtoredis/lib/log"
)

// PublishOpts are configuration options you can pass to PublishStream.
type PublishOpts struct {
	FlushInterval    time.Duration
	DedupeExpiration time.Duration
	MetadataPrefix   string
}

// This script checks whether KEYS[1] is set. If it is, it does nothing. It not,
// it sets the key, using ARGV[1] as the expiration, and then publishes the
// message ARGV[2] to channels ARGV[3] and ARGV[4].
var publishDedupe = redis.NewScript(`
	if redis.call("GET", KEYS[1]) == false then
		redis.call("SETEX", KEYS[1], ARGV[1], 1)
		redis.call("PUBLISH", ARGV[3], ARGV[2])
		redis.call("PUBLISH", ARGV[4], ARGV[2])
	end

	return true
`)

// PublishStream reads Publications from the given channel and publishes them
// to Redis.
func PublishStream(client redis.UniversalClient, in <-chan *Publication, opts *PublishOpts, stop <-chan bool) {
	// Start up a background goroutine for periodically updating the last-processed
	// timestamp
	timestampC := make(chan bson.MongoTimestamp)
	go periodicallyUpdateTimestamp(client, timestampC, opts)

	// Redis expiration is in integer seconds, so we have to convert the
	// time.Duration
	dedupeExpirationSeconds := int(opts.DedupeExpiration.Seconds())

	publishFn := func(p *Publication) error {
		return publishSingleMessage(p, client, opts.MetadataPrefix, dedupeExpirationSeconds)
	}

	for {
		select {
		case _ = <-stop:
			close(timestampC)
			return

		case p := <-in:
			err := publishSingleMessageWithRetries(p, 30, time.Second, publishFn)

			if err != nil {
				log.Log.Errorw("Permanent error while trying to publish message; giving up",
					"error", err,
					"message", p)
			} else {
				// We want to make sure we do this *after* we've successfully published
				// the messages
				timestampC <- p.OplogTimestamp
			}
		}
	}
}

func publishSingleMessageWithRetries(p *Publication, maxRetries int, sleepTime time.Duration, publishFn func(p *Publication) error) error {
	retries := 0

	for retries < maxRetries {
		err := publishFn(p)

		if err != nil {
			log.Log.Errorw("Error publishing message, will retry",
				"error", err,
				"retryNumber", retries)

			// failure, retry
			retries++
			time.Sleep(sleepTime)
		} else {
			// success, return
			return nil
		}
	}

	return fmt.Errorf("Failed to send message after retrying %d times", maxRetries)
}

func publishSingleMessage(p *Publication, client redis.UniversalClient, prefix string, dedupeExpirationSeconds int) error {
	_, err := publishDedupe.Run(
		client,
		[]string{
			// The key used for deduplication
			// The oplog timestamp isn't really a timestamp -- it's a 64-bit int
			// where the first 32 bits are a unix timestamp (seconds since
			// the epoch), and the next 32 bits are a monotonically-increasing
			// sequence number for operations within that second. It's
			// guaranteed-unique, so we can use it for deduplication
			prefix + "processed::" + encodeMongoTimestamp(p.OplogTimestamp),
		},
		dedupeExpirationSeconds, // ARGV[1], expiration time
		p.Msg,               // ARGV[2], message
		p.CollectionChannel, // ARGV[3], channel #1
		p.SpecificChannel,   // ARGV[4], channel #2
	).Result()

	return err
}

// Periodically updates the last-processed-entry timestamp in Redis.
// PublishStream sends the timestamp for *every* entry it processes to the
// channel, and this function throttles that to only update occasionally.
//
// This blocks forever; it should be run in a goroutine
func periodicallyUpdateTimestamp(client redis.UniversalClient, timestamps <-chan bson.MongoTimestamp, opts *PublishOpts) {
	var lastFlush time.Time
	var mostRecentTimestamp bson.MongoTimestamp
	var needFlush bool

	flush := func() {
		if needFlush {
			client.Set(opts.MetadataPrefix+"lastProcessedEntry", encodeMongoTimestamp(mostRecentTimestamp), 0)
			lastFlush = time.Now()
			needFlush = false
		}
	}

	for {
		select {
		case timestamp, ok := <-timestamps:
			if !ok {
				// channel got closed
				return
			}

			mostRecentTimestamp = timestamp
			needFlush = true

			if time.Now().Sub(lastFlush) > opts.FlushInterval {
				flush()
			}
		case <-time.After(opts.FlushInterval):
			if needFlush {
				flush()
			}
		}
	}
}
