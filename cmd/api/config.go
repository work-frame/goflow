package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/work-frame/goflow/internal/auth"
)

// The bcrypt cost range the server accepts from the environment.
// Below 10, stolen hashes are too cheap to crack. Each step doubles the time,
// so 14 is already about 7 s per login on a laptop.
const (
	minBcryptCost = 10
	maxBcryptCost = 14
)

// config is everything the API server reads from its environment.
// It is validated once, at startup, so run can trust it.
type config struct {
	Addr             string
	DatabaseURL      string // holds the database password: never log it
	BcryptCost       int
	RegistrationOpen bool
}

// loadConfig reads the API settings through getenv (os.Getenv in production,
// a map in tests). It reports every problem at once, not just the first.
//
// Defaults are the safe choice for a server on the internet: registration
// is closed, and a remote database must be reached over TLS.
func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		Addr:        getenv("GOFLOW_ADDR"),
		DatabaseURL: getenv("GOFLOW_DATABASE_URL"),
		BcryptCost:  auth.DefaultCost,
	}
	var errs []error

	// Hosting platforms such as Render say which port to listen on in PORT.
	// GOFLOW_ADDR still wins when both are set.
	if cfg.Addr == "" {
		if port := getenv("PORT"); port != "" {
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				errs = append(errs, fmt.Errorf("PORT must be a port number, got %q", port))
			}
			cfg.Addr = ":" + port
		} else {
			cfg.Addr = ":8080"
		}
	}

	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("GOFLOW_DATABASE_URL is required"))
	} else if err := checkDatabaseTLS(cfg.DatabaseURL); err != nil {
		errs = append(errs, err)
	}

	if raw := getenv("GOFLOW_BCRYPT_COST"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("GOFLOW_BCRYPT_COST must be a whole number, got %q", raw))
		} else {
			cfg.BcryptCost = n
		}
	}
	if cfg.BcryptCost < minBcryptCost || cfg.BcryptCost > maxBcryptCost {
		errs = append(errs, fmt.Errorf("GOFLOW_BCRYPT_COST must be between %d and %d, got %d",
			minBcryptCost, maxBcryptCost, cfg.BcryptCost))
	}

	switch reg := getenv("GOFLOW_REGISTRATION"); reg {
	case "", "closed":
		cfg.RegistrationOpen = false
	case "open":
		cfg.RegistrationOpen = true
	default:
		errs = append(errs, fmt.Errorf(`GOFLOW_REGISTRATION must be "open" or "closed", got %q`, reg))
	}

	if err := errors.Join(errs...); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// checkDatabaseTLS refuses a database on another machine unless the URL asks
// for an encrypted connection. Without it the database password and every row
// would cross the internet in plain text. A database on this machine
// (localhost, 127.0.0.1, ::1) may skip TLS, as the development container does.
// Errors never include the URL: it contains the password.
func checkDatabaseTLS(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("GOFLOW_DATABASE_URL must look like postgres://user:password@host/database")
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	switch u.Query().Get("sslmode") {
	case "require", "verify-ca", "verify-full":
		return nil
	}
	return fmt.Errorf("GOFLOW_DATABASE_URL points at %s, a remote database, so it needs sslmode=verify-full (or require)", host)
}
