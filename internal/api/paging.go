package api

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

var errInvalidCursor = errors.New("invalid cursor")

// parseLimit reads the ?limit= parameter. An empty value means job.DefaultPageSize.
func parseLimit(s string) (int, error) {
	if s == "" {
		return job.DefaultPageSize, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > job.MaxPageSize {
		return 0, fmt.Errorf("limit must be a whole number from 1 to %d", job.MaxPageSize)
	}
	return n, nil
}

// encodeCursor turns a position into an opaque string for clients.
// Inside it is base64url("<unix microseconds>:<job id>"), but clients must treat it
// as a token to pass back unchanged: the format can change without notice.
func encodeCursor(c job.Cursor) string {
	raw := strconv.FormatInt(c.CreatedAt.UnixMicro(), 10) + ":" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor reverses encodeCursor. Cursors come from clients, so validate everything.
func decodeCursor(s string) (job.Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return job.Cursor{}, errInvalidCursor
	}
	micros, id, ok := strings.Cut(string(raw), ":")
	if !ok {
		return job.Cursor{}, errInvalidCursor
	}
	n, err := strconv.ParseInt(micros, 10, 64)
	if err != nil {
		return job.Cursor{}, errInvalidCursor
	}
	if len(id) != 32 {
		return job.Cursor{}, errInvalidCursor
	}
	if _, err := hex.DecodeString(id); err != nil {
		return job.Cursor{}, errInvalidCursor
	}
	return job.Cursor{CreatedAt: time.UnixMicro(n).UTC(), ID: id}, nil
}
