package main

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// testConfig is a valid config for tests. It uses bcrypt's minimum cost,
// which loadConfig would refuse: tests skip loadConfig and call run directly.
func testConfig(addr, dbURL string) config {
	return config{Addr: addr, DatabaseURL: dbURL, BcryptCost: bcrypt.MinCost}
}

// env turns a map into a getenv function.
func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

const testDBURL = "postgres://u:secret-db-password@localhost/db"

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{"GOFLOW_DATABASE_URL": testDBURL}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	// A literal 12, not auth.DefaultCost: lowering the constant must fail this test.
	if cfg.BcryptCost != 12 {
		t.Errorf("BcryptCost = %d, want 12", cfg.BcryptCost)
	}
}

func TestLoadConfigReadsEverySetting(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"GOFLOW_ADDR":         "127.0.0.1:9000",
		"GOFLOW_DATABASE_URL": testDBURL,
		"GOFLOW_BCRYPT_COST":  "11",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := config{Addr: "127.0.0.1:9000", DatabaseURL: testDBURL, BcryptCost: 11}
	if cfg != want {
		t.Errorf("config = %+v, want %+v", cfg, want)
	}
}

func TestLoadConfigRejectsBadSettings(t *testing.T) {
	tests := []struct {
		name    string
		vars    map[string]string
		wantErr []string
	}{
		{"no database URL", map[string]string{}, []string{"GOFLOW_DATABASE_URL is required"}},
		{"cost below 10", map[string]string{"GOFLOW_DATABASE_URL": testDBURL, "GOFLOW_BCRYPT_COST": "9"}, []string{"between 10 and 14, got 9"}},
		{"bcrypt minimum", map[string]string{"GOFLOW_DATABASE_URL": testDBURL, "GOFLOW_BCRYPT_COST": "4"}, []string{"got 4"}},
		{"cost above 14", map[string]string{"GOFLOW_DATABASE_URL": testDBURL, "GOFLOW_BCRYPT_COST": "15"}, []string{"got 15"}},
		{"cost not a number", map[string]string{"GOFLOW_DATABASE_URL": testDBURL, "GOFLOW_BCRYPT_COST": "twelve"}, []string{`whole number, got "twelve"`}},
		{"every problem reported", map[string]string{"GOFLOW_BCRYPT_COST": "3"}, []string{"GOFLOW_DATABASE_URL is required", "got 3"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfig(env(tc.vars))
			if err == nil {
				t.Fatal("loadConfig succeeded, want an error")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestLoadConfigErrorsNeverContainTheDatabaseURL(t *testing.T) {
	_, err := loadConfig(env(map[string]string{"GOFLOW_DATABASE_URL": testDBURL, "GOFLOW_BCRYPT_COST": "nope"}))
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "secret-db-password") {
		t.Errorf("error leaks the database password: %v", err)
	}
}
