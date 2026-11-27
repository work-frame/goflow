// Command contactform is an example, not a service: a contact form whose
// backend hands each message to GoFlow as a signed webhook job, using an
// API key that never leaves the server (Task 89).
//
//	read -rs CONTACT_GOFLOW_KEY && export CONTACT_GOFLOW_KEY
//	go run ./examples/contactform
//
// Then open http://127.0.0.1:8090. See README.md in this folder: it runs on
// this machine only, with made-up data, and is not a public contact form.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Defaults: the hosted GoFlow API and the example receiver from Task 67.
const (
	defaultGoFlowURL  = "https://goflow-api-hewh.onrender.com"
	defaultWebhookURL = "https://goflow-receiver.onrender.com/hook"
	defaultAddr       = "127.0.0.1:8090"
)

// keyFormat is a GoFlow API key: gfk_ and 43 characters of base64url.
var keyFormat = regexp.MustCompile(`^gfk_[A-Za-z0-9_-]{43}$`)

type config struct {
	Key        string // the gfk_ key; sent only in the Authorization header to GoFlowURL
	GoFlowURL  string // where the API is, without a trailing slash
	WebhookURL string // where every message is delivered; fixed here, never taken from a request
	Addr       string // where this example listens: loopback only
}

// loadConfig reads and checks the settings. Its errors name the setting,
// never its value: the value might be the key, pasted into the wrong place.
func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		Key:        strings.TrimSpace(getenv("CONTACT_GOFLOW_KEY")),
		GoFlowURL:  orDefault(getenv("CONTACT_GOFLOW_URL"), defaultGoFlowURL),
		WebhookURL: orDefault(getenv("CONTACT_WEBHOOK_URL"), defaultWebhookURL),
		Addr:       orDefault(getenv("CONTACT_ADDR"), defaultAddr),
	}
	if cfg.Key == "" {
		return config{}, errors.New("CONTACT_GOFLOW_KEY is required: a jobs:create key from the dashboard (API keys)")
	}
	if !keyFormat.MatchString(cfg.Key) {
		return config{}, errors.New("CONTACT_GOFLOW_KEY isn't a whole GoFlow key (gfk_ and 43 characters); copy it again")
	}
	base, err := checkGoFlowURL(cfg.GoFlowURL)
	if err != nil {
		return config{}, err
	}
	cfg.GoFlowURL = base
	if err := checkWebhookURL(cfg.WebhookURL); err != nil {
		return config{}, err
	}
	if err := checkAddr(cfg.Addr); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func orDefault(v, def string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return def
}

func isLoopbackName(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkGoFlowURL allows https, or http to this machine (for tests), with
// no user, query or fragment. It returns the URL without a trailing slash.
func checkGoFlowURL(raw string) (string, error) {
	const msg = "CONTACT_GOFLOW_URL must be https://host (http:// only for 127.0.0.1 or localhost), with no user, query or fragment"
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New(msg)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopbackName(u.Hostname()):
	default:
		return "", errors.New(msg)
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

// checkWebhookURL allows only https with a host and no user or fragment.
// GoFlow checks the rest (public address, port 443) when the job is made.
func checkWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("CONTACT_WEBHOOK_URL must be an https:// URL with no user or fragment")
	}
	return nil
}

// checkAddr allows only loopback addresses: this example is not for the internet.
func checkAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || !isLoopbackName(host) {
		return errors.New("CONTACT_ADDR must be a loopback address such as 127.0.0.1:8090: this example is not a public service")
	}
	return nil
}

// newServer sets every timeout. A slow or stalled client can't hold a
// connection open forever, even on localhost. WriteTimeout covers the
// GoFlow call (goflowTimeout) with room to spare.
func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      goflowTimeout + 20*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("contact form stopped", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	a, err := newApp(cfg, logger, nil)
	if err != nil {
		return err
	}
	srv := newServer(cfg.Addr, a.routes())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	logger.Info("contact form listening", "url", "http://"+cfg.Addr, "goflow", cfg.GoFlowURL, "webhook", cfg.WebhookURL)

	select {
	case err := <-errc:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
