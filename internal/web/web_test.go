package web

import (
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
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
		case strings.HasPrefix(arg, `"/keys")`): // the key list: names and hints, never secrets (Task 87)
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
	// The drawer, the delete confirmation, the key dialog and the revoke
	// confirmation (Task 87).
	if strings.Count(html, "<dialog") != 4 || !strings.Contains(js, ".showModal()") {
		t.Error("the drawer, the delete confirmation and the two key dialogs must be modal <dialog>s")
	}
}

// Task 77: the password can be shown while typing, through a real button
// with a label that says what it does, and it's hidden again at every
// login attempt and at logout, so it's never left on screen. Case 69 is
// login() forgetting to hide it.
func TestPasswordIsShownOnlyWhileTyping(t *testing.T) {
	html, js := read(t, "static/index.html"), read(t, "static/app.js")
	if !regexp.MustCompile(`<input id="password" type="password"`).MatchString(html) {
		t.Error(`the password field doesn't start hidden (type="password")`)
	}
	toggle := regexp.MustCompile(`<button id="password-toggle"[^>]*>`).FindString(html)
	for _, want := range []string{`type="button"`, `aria-controls="password"`, `aria-label="Show password"`} {
		if !strings.Contains(toggle, want) {
			t.Errorf("the show-password button lacks %s: %q", want, toggle)
		}
	}
	for _, want := range []string{`"Hide password"`, `"Show password"`, `"text" : "password"`} {
		if !strings.Contains(js, want) {
			t.Errorf("setPasswordVisible no longer sets %s", want)
		}
	}
	body := func(signature string) string {
		i := strings.Index(js, signature)
		if i < 0 {
			t.Fatalf("app.js has no %q", signature)
		}
		end := strings.Index(js[i:], "\n}\n")
		return js[i : i+end]
	}
	login := body("async function login(e) {")
	if !strings.Contains(login, "setPasswordVisible(false)") ||
		strings.Index(login, "setPasswordVisible(false)") > strings.Index(login, "return;") {
		t.Error("login() doesn't hide the password before anything can return: a failed attempt would leave it on screen")
	}
	if !strings.Contains(body("async function logout("), "setPasswordVisible(false)") {
		t.Error("logout() doesn't hide the password")
	}
}

// Task 79 (U3): a click anywhere on a job's row opens its drawer, but a
// click that starts on a button is only that button's: Retry, Delete and
// Copy ID must never open the drawer as well. The title stays a real
// button, so the keyboard keeps a way in. Case 71 is the row handler
// without that check.
func TestRowClickLeavesButtonsAlone(t *testing.T) {
	js := read(t, "static/app.js")
	i := strings.Index(js, "function jobRow(job) {")
	if i < 0 {
		t.Fatal("app.js has no jobRow")
	}
	row := js[i : i+strings.Index(js[i:], "\n}\n")]
	handler := regexp.MustCompile(`(?s)tr\.addEventListener\("click", \(e\) => \{(.*?)\n  \}\);`).FindStringSubmatch(row)
	if handler == nil {
		t.Fatal("jobRow no longer opens the drawer from a row click")
	}
	guard, open := strings.Index(handler[1], `if (e.target.closest("button")) return;`), strings.Index(handler[1], "openDrawer(job.id)")
	if guard < 0 || open < 0 || guard > open {
		t.Error("the row's click handler opens the drawer without first ignoring clicks on buttons: Retry, Delete and Copy ID would open it too")
	}
	if !strings.Contains(row, `button(job.title, "link", () => openDrawer(job.id)`) {
		t.Error("the job title is no longer a button that opens the drawer: keyboard and screen-reader users would lose their way in")
	}
}

// Task 79 (U4): the drawer has four labelled sections, for every job type;
// a job that isn't a webhook says why it has no attempt history instead
// of drawing one.
func TestDrawerHasFourSections(t *testing.T) {
	html, js := read(t, "static/index.html"), read(t, "static/app.js")
	for _, h := range []string{">Overview</h3>", ">Execution</h3>", ">Attempt history</h3>", ">Actions</h3>"} {
		if !strings.Contains(html, h) {
			t.Errorf("the drawer has no %s section", strings.TrimSuffix(strings.TrimPrefix(h, ">"), "</h3>"))
		}
	}
	if strings.Contains(js, `$("drawer-attempts").hidden = job.type !== "webhook"`) {
		t.Error("the attempt history section is hidden for demo jobs again")
	}
	if !strings.Contains(js, "Delivery attempts are recorded for webhook jobs.") {
		t.Error("a demo job's drawer no longer explains why it has no attempt history")
	}
}

// Task 79: copying an ID is a real button with a label that names the
// full ID, it reports success or failure on itself and to screen readers,
// and a refused clipboard is handled, not thrown.
func TestCopyIDGivesFeedback(t *testing.T) {
	html, js := read(t, "static/index.html"), read(t, "static/app.js")
	for _, want := range []string{`setAttribute("aria-label", "Copy job ID " + id)`, `"Copied"`, `"Copy failed"`,
		"await navigator.clipboard.writeText(id);\n  } catch {", "announce(ok ?"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if !strings.Contains(html, `<p id="announce" class="sr-only" aria-live="polite"></p>`) {
		t.Error("index.html has no polite live region for announcements")
	}
}

// codeOnly is app.js without its comment lines, for tests that look for
// words the comments use to explain the rules.
func codeOnly(js string) string {
	var out []string
	for _, line := range strings.Split(js, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "//") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// funcBody returns the text of a top-level function in app.js.
func funcBody(t *testing.T, js, signature string) string {
	t.Helper()
	i := strings.Index(js, signature)
	if i < 0 {
		t.Fatalf("app.js has no %q", signature)
	}
	return js[i : i+strings.Index(js[i:], "\n}\n")]
}

// Task 87 (V4, conditions 1 and 3): a new key's secret lives in keySecret
// and the read-only field, and nowhere else: never in storage, a URL, the
// console, an error message or an announcement. It reaches the clipboard
// only in copyKey, run by the Copy button, and it is only ever taken from
// a successful answer that looks like a key.
func TestKeySecretStaysInThePage(t *testing.T) {
	js := codeOnly(read(t, "static/app.js"))
	if strings.Contains(js, "localStorage") || strings.Contains(js, "console.") {
		t.Error("app.js uses localStorage or the console")
	}
	if n := strings.Count(js, "sessionStorage.setItem("); n != 1 || !strings.Contains(js, "sessionStorage.setItem(TOKEN_KEY, data.token)") {
		t.Errorf("app.js writes to sessionStorage %d times; only the login token may be stored", n)
	}
	allowed := []string{
		`let keySecret = "";`,
		`keySecret = "";`,
		`keySecret = data.secret;`,
		`$("key-secret").value = keySecret;`,
		`await navigator.clipboard.writeText(keySecret);`,
		`if (keySecret) e.preventDefault();`,
		`if (keySecret && !keySaved) {`,
	}
	for _, line := range strings.Split(js, "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "//") || !strings.Contains(code, "keySecret") {
			continue
		}
		if !slices.Contains(allowed, code) {
			t.Errorf("app.js uses the key's secret in a new way: %q", code)
		}
	}
	if !strings.Contains(funcBody(t, js, "async function copyKey() {"), "writeText(keySecret)") {
		t.Error("copyKey doesn't copy the secret")
	}
	if !strings.Contains(js, `$("key-copy").addEventListener("click", copyKey);`) || strings.Count(js, "copyKey") != 2 {
		t.Error("copyKey must run from the Copy button's click and nowhere else")
	}
	create := funcBody(t, js, "async function createKey(e) {")
	check, take := strings.Index(create, `/^gfk_[A-Za-z0-9_-]{43}$/.test(data.secret)`), strings.Index(create, "keySecret = data.secret;")
	if check < 0 || take < 0 || check > take {
		t.Error("createKey shows a secret without first checking the answer holds a real key")
	}
	if strings.Contains(create, "catch") && strings.Index(create, "keySecret = data.secret;") < strings.Index(create, "} catch") {
		t.Error("createKey sets the secret before the request can have failed")
	}
}

// Task 87 (V4): once the secret is shown, "I've saved it" is the only way
// out. Escape is refused, a browser that closes the dialog anyway gets it
// opened again, and a click outside does nothing.
func TestKeyDialogClosesOnlyThroughItsButton(t *testing.T) {
	html, js := read(t, "static/index.html"), read(t, "static/app.js")
	for _, want := range []string{
		`$("key-dialog").addEventListener("cancel", (e) => {`,
		`if (keySecret) e.preventDefault();`,
		`if (keySecret && !keySaved) {` + "\n" + `      $("key-dialog").showModal();`,
		`$("key-saved").addEventListener("click", closeKeyDialog);`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if strings.Contains(js, `$("key-dialog").addEventListener("click"`) {
		t.Error("a click on the key dialog's backdrop must not close it")
	}
	reveal := html[strings.Index(html, `<section id="key-reveal"`):]
	reveal = reveal[:strings.Index(reveal, "</section>")]
	if strings.Contains(reveal, `method="dialog"`) || strings.Count(reveal, "<button") != 2 {
		t.Error(`the secret step must have exactly two buttons, Copy and "I've saved it", and no form that closes the dialog`)
	}
	if !strings.Contains(html, `<input id="key-secret" class="mono" type="text" readonly autocomplete="off" spellcheck="false"`) {
		t.Error("the secret field must be read-only, with autocomplete and spellcheck off")
	}
}

// Task 87 (condition 2): closing the dialog forgets the secret: the field,
// the variable and the copy status. Case 74 is the field left filled.
func TestKeySecretIsClearedWhenTheDialogCloses(t *testing.T) {
	js := read(t, "static/app.js")
	clear := funcBody(t, js, "function clearKeySecret() {")
	for _, want := range []string{`keySecret = "";`, `$("key-secret").value = "";`, `$("key-copy-status").textContent = "";`, `$("key-reveal").hidden = true;`} {
		if !strings.Contains(clear, want) {
			t.Errorf("clearKeySecret lacks %q: the secret would stay in the page after the dialog closes", want)
		}
	}
	closeFn := funcBody(t, js, "function closeKeyDialog() {")
	if c, x := strings.Index(closeFn, "clearKeySecret();"), strings.Index(closeFn, `$("key-dialog").close()`); c < 0 || x < 0 || c > x {
		t.Error("closeKeyDialog doesn't clear the secret before closing")
	}
	if !strings.Contains(funcBody(t, js, "async function logout("), "closeKeyDialog();") {
		t.Error("logout doesn't close the key dialog and clear its secret")
	}
}

// Task 87 (V6): the job drawer says who made a job: a key by name and
// hint, or a logged-in session.
func TestDrawerSaysWhoCreatedTheJob(t *testing.T) {
	js := read(t, "static/app.js")
	for _, want := range []string{`field(overview, "Created by", createdBy(job));`, `"API key: " + k.name + " (gfk_…" + k.hint + ")"`, `"You, logged in (dashboard or CLI)"`} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}
