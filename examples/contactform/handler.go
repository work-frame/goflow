package main

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

//go:embed form.html thanks.html style.css
var files embed.FS

// Limits, in characters (Unicode code points), not bytes.
const (
	maxName    = 100
	maxEmail   = 254
	maxMessage = 2000
	// maxFormBytes caps the request body. A form at every limit, written
	// in 4-byte characters and percent-encoded (3 bytes per byte), is
	// about 28 KiB, so 32 KiB never refuses a valid message.
	maxFormBytes = 32 << 10
)

// What the visitor reads. GoFlow's own answers are never shown.
const (
	msgExpired   = "This form has expired. Reload the page and send it again."
	msgChanged   = "This form was already sent with different text. Reload the page to send a new message."
	msgTooMany   = "Too many messages right now. Try again in a minute."
	msgNotSent   = "Your message couldn't be sent. Try again in a moment."
	msgBadForm   = "The form couldn't be read. Reload the page and try again."
	msgTooLarge  = "The form is too large."
	honeypotName = "website"
)

type app struct {
	goflow *goflow
	tokens *tokens
	log    *slog.Logger
	pages  *template.Template
}

func newApp(cfg config, logger *slog.Logger, client *http.Client) (*app, error) {
	pages, err := template.ParseFS(files, "form.html", "thanks.html")
	if err != nil {
		return nil, err
	}
	return &app{goflow: newGoFlow(cfg, client), tokens: newTokens(), log: logger, pages: pages}, nil
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.form)
	mux.HandleFunc("POST /send", a.send)
	mux.HandleFunc("GET /thanks", a.thanks)
	mux.HandleFunc("GET /style.css", a.style)
	return secureHeaders(mux)
}

// secureHeaders: no script may run, nothing may be loaded from elsewhere,
// the form may post only here, no other site may frame the page, and
// nothing is cached (the form carries a token).
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

type formPage struct {
	Token, Name, Email, Message, Error string
}

func (a *app) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := a.pages.ExecuteTemplate(w, name, data); err != nil {
		a.log.Error("render", "page", name, "err", err)
	}
}

func (a *app) form(w http.ResponseWriter, r *http.Request) {
	a.render(w, http.StatusOK, "form.html", formPage{Token: a.tokens.issue()})
}

func (a *app) thanks(w http.ResponseWriter, r *http.Request) {
	a.render(w, http.StatusOK, "thanks.html", nil)
}

func (a *app) style(w http.ResponseWriter, r *http.Request) {
	css, _ := files.ReadFile("style.css")
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Write(css)
}

// send reads exactly five fields from the posted form: token, website (the
// honeypot), name, email and message. Nothing else in the request, query
// string included, reaches the job: GoFlow's job type, title, webhook URL,
// headers and retries are decided by this program alone.
func (a *app) send(w http.ResponseWriter, r *http.Request) {
	errorPage := func(status int, msg string) {
		a.render(w, status, "form.html", formPage{Error: msg})
	}
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/x-www-form-urlencoded" {
		errorPage(http.StatusUnsupportedMediaType, msgBadForm)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			errorPage(http.StatusRequestEntityTooLarge, msgTooLarge)
			return
		}
		errorPage(http.StatusBadRequest, msgBadForm)
		return
	}
	field := func(name string) (string, bool) {
		v := r.PostForm[name]
		if len(v) > 1 {
			return "", false
		}
		if len(v) == 0 {
			return "", true
		}
		return v[0], true
	}
	token, ok1 := field("token")
	trap, ok2 := field(honeypotName)
	name, ok3 := field("name")
	email, ok4 := field("email")
	text, ok5 := field("message")
	if !(ok1 && ok2 && ok3 && ok4 && ok5) {
		errorPage(http.StatusBadRequest, msgBadForm)
		return
	}

	id, ok := a.tokens.check(token)
	if !ok {
		a.log.Info("bad token: nothing sent")
		errorPage(http.StatusBadRequest, msgExpired)
		return
	}
	if trap != "" {
		// A person never sees this field; a bot fills it. Answer as if it
		// worked, so the bot learns nothing, and send nothing.
		a.log.Info("honeypot: dropped, nothing sent")
		http.Redirect(w, r, "/thanks", http.StatusSeeOther)
		return
	}

	m, problem := validate(name, email, text)
	if problem != "" {
		a.render(w, http.StatusBadRequest, "form.html", formPage{Token: token, Name: name, Email: email, Message: text, Error: problem})
		return
	}

	// A failure that may be retried shows the form again with the same
	// token and text: sending it again reuses the same Idempotency-Key, so
	// a job GoFlow made before the answer got lost isn't made twice.
	retry := func(status int, msg string) {
		a.render(w, status, "form.html", formPage{Token: token, Name: m.Name, Email: m.Email, Message: m.Message, Error: msg})
	}
	// The visitor leaving doesn't cancel the call: its outcome is logged either way.
	res, err := a.goflow.send(context.WithoutCancel(r.Context()), id, m)
	switch {
	case err != nil:
		a.log.Warn("goflow unreachable")
		retry(http.StatusBadGateway, msgNotSent)
	case res.Status == http.StatusCreated || res.Status == http.StatusOK:
		a.log.Info("message accepted", "job_id", res.JobID, "replayed", res.Replayed)
		http.Redirect(w, r, "/thanks", http.StatusSeeOther)
	case res.Status == http.StatusUnprocessableEntity:
		a.log.Warn("goflow refused", "status", res.Status)
		errorPage(http.StatusConflict, msgChanged)
	case res.Status == http.StatusTooManyRequests:
		a.log.Warn("goflow refused", "status", res.Status)
		retry(http.StatusTooManyRequests, msgTooMany)
	default:
		a.log.Warn("goflow refused", "status", res.Status)
		retry(http.StatusBadGateway, msgNotSent)
	}
}

// validate checks the three fields and returns them cleaned, or the reason
// in words for the visitor. Lengths are counted in characters, so "Ọlá"
// is 3, not 5.
func validate(name, email, text string) (message, string) {
	for _, s := range []string{name, email, text} {
		if !utf8.ValidString(s) {
			return message{}, "The form contains text that isn't valid UTF-8."
		}
	}
	name = strings.TrimSpace(name)
	email = strings.TrimSpace(email)
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))

	switch n := utf8.RuneCountInString(name); {
	case n == 0:
		return message{}, "Enter your name."
	case n > maxName:
		return message{}, "Your name can be at most 100 characters."
	case hasControl(name, false):
		return message{}, "Your name contains a character that isn't allowed."
	}
	if !validEmail(email) {
		return message{}, "Enter an email address like name@example.com."
	}
	switch n := utf8.RuneCountInString(text); {
	case n == 0:
		return message{}, "Write a message."
	case n > maxMessage:
		return message{}, "Your message can be at most 2,000 characters."
	case hasControl(text, true):
		return message{}, "Your message contains a character that isn't allowed."
	}
	return message{Name: name, Email: email, Message: text}, ""
}

// hasControl reports a control or format character (Cc, Cf: NUL, escape
// codes, bidirectional overrides). Messages may keep newlines and tabs.
func hasControl(s string, lines bool) bool {
	for _, c := range s {
		if lines && (c == '\n' || c == '\t') {
			continue
		}
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
			return true
		}
	}
	return false
}

// validEmail is a shape check, not proof the address exists: one @,
// something on each side, a dot in the domain, no spaces or control
// characters, at most 254 characters.
func validEmail(s string) bool {
	if s == "" || utf8.RuneCountInString(s) > maxEmail || hasControl(s, false) || strings.ContainsFunc(s, unicode.IsSpace) {
		return false
	}
	local, domain, ok := strings.Cut(s, "@")
	return ok && local != "" && domain != "" && !strings.Contains(domain, "@") &&
		strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}
