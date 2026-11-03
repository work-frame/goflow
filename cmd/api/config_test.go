package main

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// testConfig is a valid config for tests. It uses bcrypt's minimum cost,
// which loadConfig would refuse: tests skip loadConfig and call run directly.
func testConfig(addr, dbURL string) config {
	return config{Addr: addr, DatabaseURL: dbURL, BcryptCost: bcrypt.MinCost, RegistrationOpen: true, SimulatedWork: 10 * time.Millisecond}
}

// env turns a map into a getenv function.
func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

const (
	testDBURL      = "postgres://u:secret-db-password@localhost/db"
	sampleRedisURL = "redis://:secret-redis-password@localhost:6379/0"
)

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
	if cfg.RegistrationOpen {
		t.Error("registration is open by default; a public server must start closed")
	}
	if cfg.RedisURL != "" {
		t.Errorf("RedisURL = %q, want empty: the queue is optional", cfg.RedisURL)
	}
	if cfg.Workers != 0 {
		t.Errorf("Workers = %d, want 0: the API runs no workers unless asked", cfg.Workers)
	}
}

func TestLoadConfigReadsEverySetting(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{
		"GOFLOW_ADDR":         "127.0.0.1:9000",
		"GOFLOW_DATABASE_URL": testDBURL,
		"GOFLOW_BCRYPT_COST":  "11",
		"GOFLOW_REGISTRATION": "open",
		"GOFLOW_REDIS_URL":    sampleRedisURL,
		"GOFLOW_WORKERS":      "3",
	}))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := config{Addr: "127.0.0.1:9000", DatabaseURL: testDBURL, RedisURL: sampleRedisURL, BcryptCost: 11, RegistrationOpen: true,
		Workers: 3, SimulatedWork: 5 * time.Second}
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
		{"bad registration value", map[string]string{"GOFLOW_DATABASE_URL": testDBURL, "GOFLOW_REGISTRATION": "yes"}, []string{`"open" or "closed", got "yes"`}},
		{"bad port", map[string]string{"GOFLOW_DATABASE_URL": testDBURL, "PORT": "http"}, []string{`PORT must be a port number, got "http"`}},
		{"workers without Redis", map[string]string{"GOFLOW_DATABASE_URL": testDBURL, "GOFLOW_WORKERS": "2"}, []string{"GOFLOW_WORKERS needs GOFLOW_REDIS_URL"}},
		{"too many workers", map[string]string{"GOFLOW_DATABASE_URL": testDBURL, "GOFLOW_REDIS_URL": sampleRedisURL, "GOFLOW_WORKERS": "33"}, []string{`from 0 to 32, got "33"`}},
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

func TestLoadConfigUsesPORTFromTheHost(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{"GOFLOW_DATABASE_URL": testDBURL, "PORT": "10000"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":10000" {
		t.Errorf("Addr = %q, want :10000", cfg.Addr)
	}

	cfg, err = loadConfig(env(map[string]string{"GOFLOW_DATABASE_URL": testDBURL, "PORT": "10000", "GOFLOW_ADDR": "127.0.0.1:9000"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != "127.0.0.1:9000" {
		t.Errorf("Addr = %q, want GOFLOW_ADDR to win over PORT", cfg.Addr)
	}
}

// A database on another machine must be reached over TLS. One on this machine
// (the development container) may skip it.
func TestLoadConfigRequiresTLSForRemoteDatabases(t *testing.T) {
	tests := []struct {
		url string
		ok  bool
	}{
		{"postgres://u:pw@localhost:5432/db?sslmode=disable", true},
		{"postgres://u:pw@127.0.0.1:5432/db?sslmode=disable", true},
		{"postgres://u:pw@[::1]:5432/db", true},
		{"postgres://u:pw@db.example.com/db?sslmode=verify-full", true},
		{"postgres://u:pw@db.example.com/db?sslmode=require", true},
		{"postgres://u:pw@db.example.com/db", false},
		{"postgres://u:pw@db.example.com/db?sslmode=disable", false},
		{"postgres://u:pw@db.example.com/db?sslmode=prefer", false},
		{"not a url", false},
	}
	for _, tc := range tests {
		_, err := loadConfig(env(map[string]string{"GOFLOW_DATABASE_URL": tc.url}))
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok = %v", tc.url, err, tc.ok)
		}
		if err != nil && strings.Contains(err.Error(), "pw@") {
			t.Errorf("%s: error contains the password: %v", tc.url, err)
		}
	}
}

// Like the database: a Redis on another machine must be reached over TLS,
// which for Redis means the rediss:// scheme.
func TestLoadConfigRequiresTLSForRemoteRedis(t *testing.T) {
	tests := []struct {
		url string
		ok  bool
	}{
		{"redis://localhost:6379/0", true},
		{"redis://:pw@127.0.0.1:6379/0", true},
		{"redis://[::1]:6379", true},
		{"redis://red-abc123:6379", true}, // Render's private network
		{"rediss://default:pw@cache.example.com:6379", true},
		{"redis://default:pw@cache.example.com:6379", false},
		{"http://default:pw@localhost:6379", false},
		{"not a url", false},
	}
	for _, tc := range tests {
		_, err := loadConfig(env(map[string]string{"GOFLOW_DATABASE_URL": testDBURL, "GOFLOW_REDIS_URL": tc.url}))
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok = %v", tc.url, err, tc.ok)
		}
		if err != nil && strings.Contains(err.Error(), "pw@") {
			t.Errorf("%s: error contains the password: %v", tc.url, err)
		}
	}
}
