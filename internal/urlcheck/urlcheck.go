// Package urlcheck holds the connection-URL checks shared by GoFlow's
// programs (the API and the worker), so both refuse the same unsafe URLs.
package urlcheck

import (
	"errors"
	"fmt"
	"net"
	"net/url"
)

// DatabaseTLS refuses a database on another machine unless the URL asks
// for an encrypted connection. Without it the database password and every row
// would cross the internet in plain text. A database on this machine
// (localhost, 127.0.0.1, ::1) may skip TLS, as the development container does.
// Errors never include the URL: it contains the password.
func DatabaseTLS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("GOFLOW_DATABASE_URL must look like postgres://user:password@host/database")
	}
	host := u.Hostname()
	if isThisMachine(host) {
		return nil
	}
	switch u.Query().Get("sslmode") {
	case "require", "verify-ca", "verify-full":
		return nil
	}
	return fmt.Errorf("GOFLOW_DATABASE_URL points at %s, a remote database, so it needs sslmode=verify-full (or require)", host)
}

// RedisTLS is DatabaseTLS for Redis. TLS is chosen by the scheme:
// rediss:// (two s's) is encrypted, redis:// is plain text and only allowed
// for a Redis on this machine. Errors never include the URL.
func RedisTLS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("GOFLOW_REDIS_URL must look like redis://host:6379/0 or rediss://user:password@host:6379")
	}
	switch u.Scheme {
	case "rediss":
		return nil
	case "redis":
		if isThisMachine(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("GOFLOW_REDIS_URL points at %s, a remote Redis, so it must start with rediss:// (TLS)", u.Hostname())
	default:
		return fmt.Errorf("GOFLOW_REDIS_URL must start with redis:// or rediss://, got %q", u.Scheme)
	}
}

// isThisMachine reports whether host is the loopback interface, where
// traffic never leaves the machine and plain text is acceptable.
func isThisMachine(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
