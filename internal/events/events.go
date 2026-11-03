// Package events carries job status changes from wherever they happen (the
// API, the relay, the workers) to the browsers watching them.
//
// The path: a store commits a change in PostgreSQL, then hands it to a
// Publisher, which sends it to one Redis Pub/Sub channel in the background.
// Every API instance listens on that channel and gives each message to its
// Hub, which passes it only to the owner's open streams.
//
// Nothing here is a source of truth. Pub/Sub delivers at most once, and a
// message published while nobody listens is gone. A client therefore loads
// the job list from PostgreSQL (GET /api/v1/jobs) after its stream says
// "ready", applies only events newer than what it loaded, and loads the
// list again whenever it's told to resync or its stream drops.
package events

import (
	"encoding/json"

	"github.com/work-frame/goflow/internal/job"
)

// Channel is the Redis Pub/Sub channel every job event goes through.
const Channel = "goflow:events"

// Message types a client can receive.
const (
	// TypeReady is the first message on every stream: the server is now
	// subscribed, so the client can load its snapshot without a gap.
	TypeReady = "ready"
	// TypeChanged carries a job that was created or whose status changed.
	TypeChanged = "changed"
	// TypeDeleted carries the ID of a job that was deleted.
	TypeDeleted = "deleted"
	// TypeResync tells the client that events may have been lost (the
	// server's Redis subscription dropped and came back): load the list again.
	TypeResync = "resync"
)

// Message is what a client receives, one per SSE "data:" line.
type Message struct {
	Type string `json:"type"`
	// ID is set for TypeDeleted.
	ID string `json:"id,omitempty"`
	// Job is set for TypeChanged. Its UserID is never encoded.
	Job *job.Job `json:"job,omitempty"`
}

// envelope is what travels through Redis: the client's message plus the
// owner it's for. The owner never reaches the client.
type envelope struct {
	UserID  string          `json:"user_id"`
	Message json.RawMessage `json:"message"`
}

// encoded returns m as JSON. Message only holds strings and a job, so
// json.Marshal can't fail on it.
func encoded(m Message) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		panic("events: encode message: " + err.Error())
	}
	return b
}
