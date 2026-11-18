// Package frontend serves only the bundled desktop interface. Rebuild its ignored
// JavaScript before Go builds: npm --prefix frontend ci && npm --prefix frontend run build.
package frontend

import (
	"embed"
	"net/http"
)

//go:embed index.html styles.css dist/status.js dist/setup.js dist/rules.js dist/history.js dist/settings.js
var assets embed.FS

func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, contentType := "", ""
		switch r.URL.Path {
		case "/", "/index.html":
			path, contentType = "index.html", "text/html; charset=utf-8"
		case "/styles.css":
			path, contentType = "styles.css", "text/css; charset=utf-8"
		case "/status.js":
			path, contentType = "dist/status.js", "text/javascript; charset=utf-8"
		case "/setup.js", "/rules.js", "/history.js", "/settings.js":
			path, contentType = "dist"+r.URL.Path, "text/javascript; charset=utf-8"
		default:
			http.NotFound(w, r)
			return
		}
		body, err := assets.ReadFile(path)
		if err != nil {
			http.Error(w, "Bundled asset unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentType)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	})
}
