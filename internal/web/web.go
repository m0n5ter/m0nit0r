// Package web serves the dashboard and its front-end assets, all compiled into
// the binary so that a deployment is a single file and the dashboard works on a
// host with no outbound internet access.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
)

//go:embed index.html
var indexHTML []byte

//go:embed assets
var assetFiles embed.FS

// assetPrefix is the URL prefix the dashboard references. The embedded layout
// mirrors it, so the relative font URLs inside bootstrap-icons.min.css resolve
// without rewriting the stylesheet.
const assetPrefix = "/assets/"

// contentTypes covers the extensions actually shipped. Relying on the system
// MIME registry would be fragile: a stripped container may not map .woff2 at
// all, and a wrong type on a font makes the icons silently disappear.
var contentTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".woff":  "font/woff",
	".woff2": "font/woff2",
}

type asset struct {
	data        []byte
	etag        string
	contentType string
}

var assets = mustLoadAssets()

// Dashboard returns a handler for the single-page dashboard.
func Dashboard() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(indexHTML)))
		// The page polls for its own data, so a cached shell would only serve
		// to hide an upgrade from anyone with the tab already open.
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(indexHTML)
	})
}

// Assets returns a handler for the embedded stylesheets, scripts and fonts.
//
// Asset URLs are not versioned, so they are cached with revalidation rather
// than immutably: after an upgrade the ETag changes and the browser picks up
// the new bytes on its next conditional request instead of serving a stale
// bundle until the cache expires.
func Assets() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), assetPrefix)

		a, ok := assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("ETag", a.etag)
		w.Header().Set("Cache-Control", "public, max-age=86400, must-revalidate")

		if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, a.etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.Header().Set("Content-Type", a.contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(a.data)))
		w.Write(a.data)
	})
}

// mustLoadAssets reads every embedded asset once at start-up. A failure here is
// a build problem, not a runtime condition, so it panics rather than degrading.
func mustLoadAssets() map[string]asset {
	loaded := map[string]asset{}

	err := fs.WalkDir(assetFiles, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		data, err := assetFiles.ReadFile(p)
		if err != nil {
			return err
		}

		ext := strings.ToLower(path.Ext(p))
		contentType, ok := contentTypes[ext]
		if !ok {
			return fmt.Errorf("no content type registered for %s", p)
		}

		sum := sha256.Sum256(data)
		loaded[strings.TrimPrefix(p, "assets/")] = asset{
			data:        data,
			etag:        `"` + hex.EncodeToString(sum[:8]) + `"`,
			contentType: contentType,
		}
		return nil
	})
	if err != nil {
		panic("web: loading embedded assets: " + err.Error())
	}

	return loaded
}
