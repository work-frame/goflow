package job

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidateWebhookAcceptsHTTPSOn443(t *testing.T) {
	for _, u := range []string{
		"https://example.com/hooks/goflow",
		"https://example.com:443/hooks?token=abc",
		"HTTPS://Example.COM/x",
		"https://example.com",
	} {
		if err := ValidateWebhook(Webhook{URL: u, Body: json.RawMessage(`{"order":1042}`)}); err != nil {
			t.Errorf("ValidateWebhook(%q) = %v, want nil", u, err)
		}
	}
}

// Every refusal is an *InvalidError whose message never repeats the URL:
// the path or query of a webhook URL often holds a token.
func TestValidateWebhookRefusals(t *testing.T) {
	body := json.RawMessage(`{"ok":true}`)
	for _, c := range []struct {
		name string
		w    Webhook
		want string
	}{
		{"plain http", Webhook{URL: "http://example.com/x?secret=s3cr3t", Body: body}, "must use https"},
		{"other scheme", Webhook{URL: "ftp://example.com/x", Body: body}, "must use https"},
		{"relative", Webhook{URL: "/hooks", Body: body}, "absolute https:// URL"},
		{"opaque", Webhook{URL: "https:example.com", Body: body}, "absolute https:// URL"},
		{"no host", Webhook{URL: "https:///x", Body: body}, "absolute https:// URL"},
		{"empty port host", Webhook{URL: "https://:443/x", Body: body}, "absolute https:// URL"},
		{"user and password", Webhook{URL: "https://admin:s3cr3t@example.com/x", Body: body}, "username or password"},
		{"other port", Webhook{URL: "https://example.com:8443/x", Body: body}, "port 443"},
		{"fragment", Webhook{URL: "https://example.com/x#s3cr3t", Body: body}, "#fragment"},
		{"empty fragment", Webhook{URL: "https://example.com/x#", Body: body}, "#fragment"},
		{"loopback", Webhook{URL: "https://127.0.0.1/x", Body: body}, "private, loopback or reserved"},
		{"loopback v6", Webhook{URL: "https://[::1]/x", Body: body}, "private, loopback or reserved"},
		{"mapped loopback", Webhook{URL: "https://[::ffff:127.0.0.1]/x", Body: body}, "private, loopback or reserved"},
		{"metadata", Webhook{URL: "https://169.254.169.254/latest", Body: body}, "private, loopback or reserved"},
		{"private", Webhook{URL: "https://10.0.0.1/x", Body: body}, "private, loopback or reserved"},
		{"localhost", Webhook{URL: "https://localhost/x", Body: body}, "private, loopback or reserved"},
		{"localhost subdomain", Webhook{URL: "https://api.LOCALHOST./x", Body: body}, "private, loopback or reserved"},
		{"too long", Webhook{URL: "https://example.com/" + strings.Repeat("a", MaxWebhookURLLength), Body: body}, "at most 2048 bytes"},
		{"no body", Webhook{URL: "https://example.com/x"}, "body is required"},
		{"body too big", Webhook{URL: "https://example.com/x", Body: json.RawMessage(`"` + strings.Repeat("a", MaxWebhookBodyBytes) + `"`)}, "at most 65536 bytes"},
		{"body not JSON", Webhook{URL: "https://example.com/x", Body: json.RawMessage(`{"a":`)}, "must be JSON"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateWebhook(c.w)
			var invalid *InvalidError
			if !errors.As(err, &invalid) {
				t.Fatalf("ValidateWebhook = %v, want an *InvalidError", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q doesn't say %q", err, c.want)
			}
			if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "example.com") {
				t.Errorf("error %q repeats part of the URL", err)
			}
		})
	}
}

func TestValidateMaxAttempts(t *testing.T) {
	for n, ok := range map[int]bool{-1: false, 0: false, 1: true, 3: true, 10: true, 11: false} {
		if err := ValidateMaxAttempts(n); (err == nil) != ok {
			t.Errorf("ValidateMaxAttempts(%d) = %v, want ok=%v", n, err, ok)
		}
	}
}

func TestNewJobsAreDemoJobs(t *testing.T) {
	j, err := New("sleep:1s")
	if err != nil {
		t.Fatal(err)
	}
	if j.Type != TypeDemo || j.Webhook != nil {
		t.Errorf("New: type %q, webhook %v; want demo with no webhook", j.Type, j.Webhook)
	}
}

func TestWithoutBodyLeavesTheJobAlone(t *testing.T) {
	j := &Job{ID: "a", Type: TypeWebhook, Webhook: &Webhook{URL: "https://example.com/x", Body: json.RawMessage(`{"n":1}`)}}
	c := j.WithoutBody()
	if c.Webhook.Body != nil || c.Webhook.URL != "https://example.com/x" {
		t.Errorf("copy: %+v, want the URL without the body", c.Webhook)
	}
	if string(j.Webhook.Body) != `{"n":1}` {
		t.Errorf("WithoutBody changed the original's body to %q", j.Webhook.Body)
	}
	demo := &Job{ID: "b"}
	if demo.WithoutBody() != demo {
		t.Error("a job with no body should come back as itself")
	}
}
