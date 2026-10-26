package main

import (
	"errors"
	"fmt"
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
	Addr        string
	DatabaseURL string // holds the database password: never log it
	BcryptCost  int
}

// loadConfig reads the API settings through getenv (os.Getenv in production,
// a map in tests). It reports every problem at once, not just the first.
func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		Addr:        getenv("GOFLOW_ADDR"),
		DatabaseURL: getenv("GOFLOW_DATABASE_URL"),
		BcryptCost:  auth.DefaultCost,
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}

	var errs []error
	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("GOFLOW_DATABASE_URL is required"))
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

	if err := errors.Join(errs...); err != nil {
		return config{}, err
	}
	return cfg, nil
}
