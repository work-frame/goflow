package urlcheck

import (
	"strings"
	"testing"
)

func TestRedisTLS(t *testing.T) {
	tests := []struct {
		url string
		ok  bool
	}{
		{"redis://127.0.0.1:6379/0", true},                   // this machine
		{"redis://localhost:6379", true},                     // this machine
		{"redis://red-abc123:6379", true},                    // Render private network (single label)
		{"redis://user:pw@red-abc123:6379", true},            // same, with internal auth
		{"rediss://default:pw@cache.example.com:6379", true}, // TLS to anywhere
		{"redis://default:pw@cache.example.com:6379", false}, // plain text over the internet
		{"redis://203.0.113.7:6379", false},                  // a public IP is not a private name
		{"http://localhost:6379", false},
		{"not a url", false},
	}
	for _, tc := range tests {
		err := RedisTLS(tc.url)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok = %v", tc.url, err, tc.ok)
		}
		if err != nil && strings.Contains(err.Error(), "pw@") {
			t.Errorf("%s: error contains the password: %v", tc.url, err)
		}
	}
}

func TestDatabaseTLS(t *testing.T) {
	tests := []struct {
		url string
		ok  bool
	}{
		{"postgres://u:pw@localhost/db", true},
		{"postgres://u:pw@db.example.com/db?sslmode=verify-full", true},
		{"postgres://u:pw@db.example.com/db", false},
		// A single-label name only exists inside a private network: the
		// Compose service "postgres" (Phase 11). Neon's host has dots.
		{"postgres://u:pw@postgres:5432/db?sslmode=disable", true},
		{"postgres://u:pw@dbhost/db", true},
		{"postgres://u:pw@ep-cool-name-123.eu-central-1.aws.neon.tech/db?sslmode=disable", false},
		// A remote IP address is not a name, private or not.
		{"postgres://u:pw@10.0.0.5/db", false},
	}
	for _, tc := range tests {
		if err := DatabaseTLS(tc.url); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok = %v", tc.url, err, tc.ok)
		}
	}
}
