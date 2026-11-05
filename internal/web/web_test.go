package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/work-frame/goflow/internal/events"
)

func serve(t *testing.T, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestFilesAreServedWithSecurityHeaders(t *testing.T) {
	for path, wantType := range map[string]string{
		"/":          "text/html; charset=utf-8",
		"/app.js":    "text/javascript; charset=utf-8",
		"/style.css": "text/css; charset=utf-8",
	} {
		rec := serve(t, http.MethodGet, path)
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("GET %s: status %d, %d bytes", path, rec.Code, rec.Body.Len())
		}
		h := rec.Header()
		if h.Get("Content-Type") != wantType {
			t.Errorf("GET %s: Content-Type %q, want %q", path, h.Get("Content-Type"), wantType)
		}
		if h.Get("Content-Security-Policy") != ContentSecurityPolicy {
			t.Errorf("GET %s: CSP %q", path, h.Get("Content-Security-Policy"))
		}
		if h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: missing X-Content-Type-Options: nosniff", path)
		}
	}
}

// The policy is what stops an injected script from running or from
// sending the token away. It must never be loosened by accident.
func TestPolicyIsStrict(t *testing.T) {
	directives := map[string]string{}
	for _, d := range strings.Split(ContentSecurityPolicy, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(d), " ")
		directives[name] = value
	}
	for name, want := range map[string]string{
		"default-src":     "'none'",
		"script-src":      "'self'",
		"connect-src":     "'self'",
		"frame-ancestors": "'none'",
		"base-uri":        "'none'",
	} {
		if directives[name] != want {
			t.Errorf("%s = %q, want %q", name, directives[name], want)
		}
	}
	for _, loose := range []string{"unsafe-inline", "unsafe-eval", "*", "http:", "https:"} {
		if strings.Contains(ContentSecurityPolicy, loose) {
			t.Errorf("policy contains %q", loose)
		}
	}
}

// Only "/" exactly is the page; other paths are left to the API.
func TestOnlyTheRootIsThePage(t *testing.T) {
	if rec := serve(t, http.MethodGet, "/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope: status %d, want 404", rec.Code)
	}
	if rec := serve(t, http.MethodPost, "/"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /: status %d, want 405", rec.Code)
	}
}

// The HTML must work under the policy: scripts only from files, no inline
// event handlers or styles (the policy would block them, and an
// exception for them would let injected ones run too).
func TestPageHasNoInlineCode(t *testing.T) {
	html := read(t, "static/index.html")
	for _, m := range regexp.MustCompile(`(?is)<script\b[^>]*>`).FindAllString(html, -1) {
		if !strings.Contains(m, `src="/`) {
			t.Errorf("inline or external script: %s", m)
		}
	}
	for name, pattern := range map[string]string{
		"inline event handler": `(?i)\son[a-z]+\s*=`,
		"inline style":         `(?i)\sstyle\s*=`,
		"javascript: URL":      `(?i)javascript:`,
	} {
		if m := regexp.MustCompile(pattern).FindString(html); m != "" {
			t.Errorf("index.html has an %s: %q", name, m)
		}
	}
}

// Text from the server must never be parsed as HTML. The script builds
// every element with createElement and textContent; these are the ways
// a string would become markup or code.
func TestScriptNeverTurnsTextIntoHTML(t *testing.T) {
	js := read(t, "static/app.js")
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "setTimeout(\"", "setTimeout('"} {
		if strings.Contains(js, banned) {
			t.Errorf("app.js uses %s", banned)
		}
	}
	// The session token travels in a header only. A URL ends up in logs.
	if strings.Contains(js, "token=") {
		t.Error("app.js puts something called token in a URL")
	}
}

// Every message type the server can send has a case in app.js, and the
// two that matter most do the right thing: "paused" shows the paused dot,
// "resumed" reloads the list (events sent during the pause are gone). A
// browser test would be better; check.sh has none yet, so this reads the
// source.
func TestScriptHandlesEveryMessageType(t *testing.T) {
	js := read(t, "static/app.js")
	for _, typ := range []string{events.TypeReady, events.TypeChanged, events.TypeDeleted, events.TypeResync, events.TypePaused, events.TypeResumed} {
		if !strings.Contains(js, `case "`+typ+`":`) {
			t.Errorf("app.js has no case for %q messages", typ)
		}
	}
	caseBody := func(typ string) string {
		m := regexp.MustCompile(`(?s)case "` + typ + `":(.*?)break;`).FindStringSubmatch(js)
		if m == nil {
			return ""
		}
		return m[1]
	}
	if !strings.Contains(caseBody(events.TypePaused), `setLive("paused"`) {
		t.Error(`app.js: "paused" doesn't set the paused state`)
	}
	for _, typ := range []string{events.TypeReady, events.TypeResync, events.TypeResumed} {
		if !strings.Contains(caseBody(typ), "loadAll") {
			t.Errorf("app.js: %q doesn't reload the list", typ)
		}
	}
	if !strings.Contains(read(t, "static/style.css"), ".live.paused") {
		t.Error("style.css has no style for the paused dot")
	}
}

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
