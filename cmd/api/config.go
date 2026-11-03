package main

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/work-frame/goflow/internal/auth"
	"github.com/work-frame/goflow/internal/urlcheck"
)

// The bcrypt cost range the server accepts from the environment.
// Below 10, stolen hashes are too cheap to crack. Each step doubles the time,
// so 14 is already about 7 s per login on a laptop.
const (
	minBcryptCost = 10
	maxBcryptCost = 14
	// maxWorkers caps GOFLOW_WORKERS.
	maxWorkers = 32
	// simulatedWork is how long the placeholder job handler takes.
	simulatedWork = 5 * time.Second
)

// config is everything the API server reads from its environment.
// It is validated once, at startup, so run can trust it.
type config struct {
	Addr             string
	DatabaseURL      string // holds the database password: never log it
	RedisURL         string // optional; may hold a password: never log it
	BcryptCost       int
	RegistrationOpen bool
	// Workers is how many job workers the API runs itself (0: none).
	// Needs RedisURL. Only ONE API instance may run workers: see worker.Pool.
	Workers int
	// SimulatedWork is how long each placeholder job takes. Not read from
	// the environment; tests shorten it.
	SimulatedWork time.Duration
}

// loadConfig reads the API settings through getenv (os.Getenv in production,
// a map in tests). It reports every problem at once, not just the first.
//
// Defaults are the safe choice for a server on the internet: registration
// is closed, and a remote database must be reached over TLS.
func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		Addr:          getenv("GOFLOW_ADDR"),
		DatabaseURL:   getenv("GOFLOW_DATABASE_URL"),
		RedisURL:      getenv("GOFLOW_REDIS_URL"),
		BcryptCost:    auth.DefaultCost,
		SimulatedWork: simulatedWork,
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
	} else if err := urlcheck.DatabaseTLS(cfg.DatabaseURL); err != nil {
		errs = append(errs, err)
	}

	// Redis is optional until workers exist (Phase 6): without it, jobs are
	// saved but not queued.
	if cfg.RedisURL != "" {
		if err := urlcheck.RedisTLS(cfg.RedisURL); err != nil {
			errs = append(errs, err)
		}
	}

	if raw := getenv("GOFLOW_WORKERS"); raw != "" {
		n, err := strconv.Atoi(raw)
		switch {
		case err != nil || n < 0 || n > maxWorkers:
			errs = append(errs, fmt.Errorf("GOFLOW_WORKERS must be a whole number from 0 to %d, got %q", maxWorkers, raw))
		case n > 0 && cfg.RedisURL == "":
			errs = append(errs, errors.New("GOFLOW_WORKERS needs GOFLOW_REDIS_URL: workers take jobs from Redis"))
		default:
			cfg.Workers = n
		}
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
