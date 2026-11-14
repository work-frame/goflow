package api

import (
	"bytes"
	"encoding/json"
	"math/big"

	"github.com/work-frame/goflow/internal/job"
)

// sameWork reports whether a replayed create asks for the same work as the
// first job: same type, attempt limit, and for a webhook the same URL and
// body. The title is compared by the caller.
func sameWork(first *job.Job, req *job.Job) bool {
	if first.Type != req.Type || first.MaxAttempts != req.MaxAttempts {
		return false
	}
	if (first.Webhook == nil) != (req.Webhook == nil) {
		return false
	}
	if first.Webhook == nil {
		return true
	}
	return first.Webhook.URL == req.Webhook.URL && sameJSON(first.Webhook.Body, req.Webhook.Body)
}

// sameJSON reports whether a and b are the same JSON value. PostgreSQL's
// jsonb doesn't keep the client's bytes: it reorders keys, drops spaces and
// may write a number differently (1e2 becomes 100). So the comparison is by
// value, with numbers compared exactly, never as float64: two large IDs
// that differ only past float64's 15 digits are different requests.
func sameJSON(a, b []byte) bool {
	va, okA := decodeExact(a)
	vb, okB := decodeExact(b)
	return okA && okB && equalJSON(va, vb)
}

func decodeExact(b []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

func equalJSON(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !equalJSON(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equalJSON(x[i], y[i]) {
				return false
			}
		}
		return true
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		rx, okX := new(big.Rat).SetString(string(x))
		ry, okY := new(big.Rat).SetString(string(y))
		return okX && okY && rx.Cmp(ry) == 0
	default: // string, bool, nil
		return a == b
	}
}
