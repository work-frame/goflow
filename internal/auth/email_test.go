package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		in   string
		want string
		err  error
	}{
		{"dan@example.com", "dan@example.com", nil},
		{"  Dan@Example.COM ", "dan@example.com", nil},
		{"", "", ErrInvalidEmail},
		{"   ", "", ErrInvalidEmail},
		{"no-at-sign", "", ErrInvalidEmail},
		{"Dan <dan@example.com>", "", ErrInvalidEmail},
		{"dan@", "", ErrInvalidEmail},
		{strings.Repeat("a", 250) + "@x.io", "", ErrInvalidEmail},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := NormalizeEmail(tt.in)
			if !errors.Is(err, tt.err) {
				t.Fatalf("NormalizeEmail(%q) error = %v, want %v", tt.in, err, tt.err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeEmail(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
