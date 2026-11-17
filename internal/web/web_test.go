package web

import (
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/work-frame/goflow/internal/events"
	"github.com/work-frame/goflow/internal/job"
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

// Daniel's condition for Task 66: the page shows a webhook's host and
// nothing else of its URL, never its body, and never asks for a signing
// secret. The URL is read in one place, hostOf.
func TestScriptShowsOnlyTheWebhookHost(t *testing.T) {
	js := read(t, "static/app.js")
	if !strings.Contains(js, "hostOf(job.webhook.url)") {
		t.Error("app.js doesn't show a webhook through hostOf(job.webhook.url): the whole URL may be on the page")
	}
	if n := strings.Count(js, "webhook.url"); n != 1 {
		t.Errorf("app.js reads webhook.url %d times; only hostOf may read it", n)
	}
	for _, banned := range []string{"webhook.body", ".body)", "webhook-secret", "whsec_", ".search", ".pathname", ".href"} {
		if strings.Contains(js, banned) {
			t.Errorf("app.js contains %q", banned)
		}
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

// Task 76 (D2): the details drawer is drawn from the list's copy of a job.
// GET /api/v1/jobs/{id} answers with the webhook body, so the page never
// asks for it: every GET the script makes is the list, the caller, or a
// job's attempts. Raw fetch calls are only the API helper, login and
// /readyz. Case 68 in FAILURES.md is this test catching a drawer that
// "refreshed" the job from the single-job endpoint.
func TestDrawerNeverFetchesTheFullJob(t *testing.T) {
	js := read(t, "static/app.js")
	gets := regexp.MustCompile(`api\("GET",\s*([^\n]*)`).FindAllStringSubmatch(js, -1)
	if len(gets) == 0 {
		t.Fatal("found no GET calls in app.js: the test no longer reads the script")
	}
	for _, g := range gets {
		arg := g[1]
		switch {
		case strings.HasPrefix(arg, `"/jobs?"`), strings.HasPrefix(arg, `"/auth/me"`):
		case strings.HasPrefix(arg, `"/jobs/" +`) && strings.Contains(arg, `+ "/attempts")`):
		default:
			t.Errorf("app.js makes a GET the dashboard must not make: api(\"GET\", %s", arg)
		}
	}
	for _, f := range regexp.MustCompile(`fetch\(([^,)]*)`).FindAllStringSubmatch(js, -1) {
		switch f[1] {
		case "API + path", `API + "/auth/login"`, `"/readyz"`:
		default:
			t.Errorf("app.js calls fetch(%s) outside the api helper", f[1])
		}
	}
}

// Every status the API can send has a badge style and is in the filter.
// Color is never the only signal: the badge always has the word too.
func TestEveryStatusHasAStyle(t *testing.T) {
	js, css := read(t, "static/app.js"), read(t, "static/style.css")
	for _, s := range []job.Status{job.StatusPending, job.StatusQueued, job.StatusProcessing,
		job.StatusRetrying, job.StatusCompleted, job.StatusFailed, job.StatusDead} {
		if !s.Valid() {
			t.Fatalf("%q isn't a valid status any more: update this list", s)
		}
		if !strings.Contains(css, ".status-"+string(s)) {
			t.Errorf("style.css has no .status-%s", s)
		}
		if !strings.Contains(js, `"`+string(s)+`"`) {
			t.Errorf("app.js doesn't know the status %q", s)
		}
	}
	if !strings.Contains(js, `el("span", "", status)`) {
		t.Error("badge() no longer writes the status word next to the dot")
	}
}

// D5: dark only, WCAG AA. Every text color is checked against every
// background it is drawn on, from the tokens in style.css.
func TestColorsMeetWCAGAA(t *testing.T) {
	css := read(t, "static/style.css")
	root := css[strings.Index(css, ":root {"):]
	root = root[:strings.Index(root, "}")]
	tokens := map[string]string{}
	for _, m := range regexp.MustCompile(`--([a-z0-9-]+):\s*(#[0-9a-f]{6});`).FindAllStringSubmatch(root, -1) {
		tokens[m[1]] = m[2]
	}
	if strings.Contains(css, "prefers-color-scheme") {
		t.Error("style.css has a light theme: D5 is dark only")
	}
	backgrounds := []string{"bg", "surface", "surface-2"}
	texts := []string{"text", "muted", "accent", "placeholder", "ok", "info", "neutral", "warn", "bad"}
	pairs := [][2]string{{"accent-ink", "accent"}, {"ok", "ok-bg"}, {"info", "info-bg"},
		{"neutral", "neutral-bg"}, {"warn", "warn-bg"}, {"bad", "bad-bg"}}
	for _, fg := range texts {
		for _, bg := range backgrounds {
			pairs = append(pairs, [2]string{fg, bg})
		}
	}
	for _, p := range pairs {
		fg, bg := tokens[p[0]], tokens[p[1]]
		if fg == "" || bg == "" {
			t.Errorf("style.css :root is missing --%s or --%s", p[0], p[1])
			continue
		}
		if r := contrast(fg, bg); r < 4.5 {
			t.Errorf("--%s on --%s: contrast %.2f:1, WCAG AA needs 4.5:1", p[0], p[1], r)
		}
	}
}

func contrast(a, b string) float64 {
	lum := func(hex string) float64 {
		var c [3]float64
		for i := range c {
			var v int
			for _, ch := range hex[1+2*i : 3+2*i] {
				v = v*16 + strings.IndexRune("0123456789abcdef", ch)
			}
			x := float64(v) / 255
			if x <= 0.03928 {
				c[i] = x / 12.92
			} else {
				c[i] = math.Pow((x+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
	}
	la, lb := lum(a), lum(b)
	return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05)
}

// Someone who asks their system for less motion gets none: no shimmer,
// no pulse, no sliding drawer.
func TestReducedMotionStopsAnimation(t *testing.T) {
	css := read(t, "static/style.css")
	i := strings.Index(css, "@media (prefers-reduced-motion: reduce)")
	if i < 0 {
		t.Fatal("style.css has no prefers-reduced-motion block")
	}
	block := css[i:]
	for _, want := range []string{"animation: none !important", "transition: none !important"} {
		if !strings.Contains(block, want) {
			t.Errorf("the reduced-motion block doesn't set %q", want)
		}
	}
}

// Every form control has a label a screen reader announces, and the page
// uses its own dialogs, not the browser's blocking ones.
func TestFormControlsHaveLabels(t *testing.T) {
	html := read(t, "static/index.html")
	controls := regexp.MustCompile(`(?s)<(input|select|textarea)\b[^>]*>`).FindAllString(html, -1)
	if len(controls) < 6 {
		t.Fatalf("found %d form controls, expected at least 6", len(controls))
	}
	for _, c := range controls {
		id := regexp.MustCompile(`\bid="([^"]+)"`).FindStringSubmatch(c)
		if id == nil {
			t.Errorf("control without an id: %s", c)
			continue
		}
		if !strings.Contains(html, `for="`+id[1]+`"`) && !strings.Contains(c, "aria-label=") {
			t.Errorf("#%s has no <label for> and no aria-label", id[1])
		}
	}
	js := read(t, "static/app.js")
	for _, banned := range []string{"alert(", "confirm(", "prompt("} {
		if strings.Contains(js, banned) {
			t.Errorf("app.js uses %s: use the page's own <dialog>", banned)
		}
	}
	if strings.Count(html, "<dialog") != 2 || !strings.Contains(js, ".showModal()") {
		t.Error("the drawer and the delete confirmation must be modal <dialog>s")
	}
}
