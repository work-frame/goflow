// Package web serves the dashboard: one HTML page, one script and one
// stylesheet, embedded in the binary. The page talks to the same API as
// the CLI, from the same origin, so it needs no CORS rules.
package web

import (
	"embed"
	"net/http"
)

//go:embed static/index.html static/app.js static/style.css
var files embed.FS

// ContentSecurityPolicy tells the browser what the page may load and run.
// The page keeps a 24-hour session token in sessionStorage, where any
// script on the page can read it, so the policy is as strict as the page
// allows:
//
//   - script-src 'self': only /app.js runs. No inline <script>, no
//     onclick="..." attributes, no eval, nothing from another site. A job
//     title that somehow became HTML still couldn't run code.
//   - connect-src 'self': fetch and EventSource reach only this API, so a
//     script that did get in couldn't send the token elsewhere.
//   - default-src 'none': everything not listed (frames, fonts, objects,
//     media) is refused.
//   - frame-ancestors 'none': no other site can put the page in a frame
//     and trick clicks out of it.
const ContentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// Register adds the dashboard's three routes to mux. The page is at "/"
// exactly ("/{$}"): any other path still reaches the API's JSON 404.
func Register(mux *http.ServeMux) {
	mux.Handle("GET /{$}", page("static/index.html", "text/html; charset=utf-8"))
	mux.Handle("GET /app.js", page("static/app.js", "text/javascript; charset=utf-8"))
	mux.Handle("GET /style.css", page("static/style.css", "text/css; charset=utf-8"))
}

// page serves one embedded file with the security headers.
func page(name, contentType string) http.Handler {
	body, err := files.ReadFile(name)
	if err != nil {
		panic("web: missing embedded file " + name) // a build mistake, caught by the tests
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("Content-Security-Policy", ContentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff") // never guess a type: a .js is only ever a script
		h.Set("Referrer-Policy", "no-referrer")
		// Check with the server on every load, so a deploy is picked up at
		// once. The files are small; the API is what costs time.
		h.Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			w.Write(body)
		}
	})
}
