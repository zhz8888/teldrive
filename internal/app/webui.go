package app

import (
	"bytes"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"

	embeddedui "github.com/zhz8888/teldrive/v2/ui"
)

// newWebUIHandler returns a handler for the UI bundle compiled into the binary.
// It fails when the bundle is missing or has no index.html, which turns a build
// that forgot to compile the UI into a startup error instead of a runtime 404.
func newWebUIHandler() (http.Handler, error) {
	files, err := fs.Sub(embeddedui.StaticFS, "dist")
	if err != nil {
		return nil, fmt.Errorf("open embedded UI: %w", err)
	}
	if _, err := fs.Stat(files, "index.html"); err != nil {
		return nil, fmt.Errorf("inspect embedded UI index: %w", err)
	}
	return webUIHandler{files: files}, nil
}

// webUIHandler serves the embedded single-page application. It is a value type
// holding only a filesystem, so copies of it share the same read-only bundle.
type webUIHandler struct {
	// files is the "dist" subtree of the embedded UI filesystem, rooted where
	// Vite wrote index.html and the hashed asset files.
	files fs.FS
}

const (
	// webUIAssetsDirectory is the bundle directory Vite writes generated files into.
	// It is the default build.assetsDir of the UI's vite.config.mts, and the only
	// directory of the bundle whose names carry a content hash; fonts, images and the
	// copied pdfjs tree keep stable names.
	webUIAssetsDirectory = "assets"
	// webUIAssetHashLength is the number of hash characters Vite (through Rollup)
	// appends to a generated file name. The bundle shows exactly eight, drawn from the
	// URL-safe base64 alphabet, for example assets/index-DpMfofu4.js and
	// assets/epub-p5nIh-Lz.js, where the hash itself contains a hyphen.
	webUIAssetHashLength = 8
)

// isHashedUIAsset reports whether the served file name is a Vite content-hashed build
// artefact, which is what makes a one-year immutable cache safe: a changed asset is
// published under a new name, so the cached entry can never go stale. The check is
// deliberately narrow, because a wrong "hashed" verdict would pin a stable file for a
// year after a deployment replaces it: the name must sit in webUIAssetsDirectory and
// end in a hyphen followed by exactly webUIAssetHashLength hash characters before its
// extension. So assets/app-deadbeef.js and assets/epub-p5nIh-Lz.js cache for a year,
// while assets/logo-dark.svg, images/favicon-16x16.png and pdfjs/wasm/quickjs-eval.wasm
// do not. A future build that renames the directory or changes the hash length only
// loses the long cache; it never serves a stale file.
func isHashedUIAsset(name string) bool {
	directory, base := path.Split(name)
	if strings.Trim(directory, "/") != webUIAssetsDirectory {
		return false
	}
	stem := strings.TrimSuffix(base, path.Ext(base))
	hashStart := len(stem) - webUIAssetHashLength
	if hashStart < 1 || stem[hashStart-1] != '-' {
		return false
	}
	for _, character := range stem[hashStart:] {
		if !isUIAssetHashCharacter(character) {
			return false
		}
	}
	return true
}

// isUIAssetHashCharacter reports whether character can appear in a Vite asset hash,
// which is the URL-safe base64 alphabet introduced with the "-" and "_" characters.
func isUIAssetHashCharacter(character rune) bool {
	switch {
	case character >= '0' && character <= '9':
		return true
	case character >= 'a' && character <= 'z':
		return true
	case character >= 'A' && character <= 'Z':
		return true
	case character == '-' || character == '_':
		return true
	default:
		return false
	}
}

// ServeHTTP serves one asset of the embedded UI. Only GET and HEAD are accepted
// and everything else gets 405. Hidden paths (any dot-prefixed segment) are
// answered with 404 so the bundle cannot be walked for editor or VCS leftovers.
// A miss on an extensionless path falls back to index.html, which is what makes
// client-side routes such as /files/123 work on a hard reload; a miss on a path
// with an extension stays a 404. The response carries a strict CSP, and caching
// is split by file name: index.html is no-cache, content-hashed assets (see
// isHashedUIAsset) are immutable for a year, and the remaining files are cached for
// an hour, so a stable name such as images/favicon-16x16.png cannot outlive a
// deployment.
func (h webUIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// script-src also allows 'wasm-unsafe-eval' because the PDF reader hands
	// pdf.js a wasmUrl: WebAssembly compilation counts as an eval-like operation,
	// so without the keyword the reader loads its worker and then fails to start.
	w.Header().Set("Content-Security-Policy", "default-src 'self' blob:; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline' blob:; img-src 'self' data: blob:; media-src 'self' blob:; font-src 'self' data: blob:; connect-src 'self' data: blob:; worker-src 'self' blob:; frame-src blob: data:; object-src 'none'; base-uri 'self'; form-action 'self'")
	requested := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if requested == "." || requested == "" {
		requested = "index.html"
	}
	if strings.HasPrefix(requested, ".") || strings.Contains(requested, "/.") {
		http.NotFound(w, r)
		return
	}
	content, stat, err := readWebUIFile(h.files, requested)
	if err != nil {
		if path.Ext(requested) != "" {
			http.NotFound(w, r)
			return
		}
		content, stat, err = readWebUIFile(h.files, "index.html")
	}
	if err != nil {
		http.Error(w, "UI is unavailable", http.StatusServiceUnavailable)
		return
	}
	if contentType := mime.TypeByExtension(path.Ext(stat.Name())); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if stat.Name() == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	} else if isHashedUIAsset(requested) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	http.ServeContent(w, r, stat.Name(), stat.ModTime(), content)
}

// readWebUIFile loads name from the embedded bundle in full and returns its
// bytes, its FileInfo and any error. The whole file is buffered because
// http.ServeContent needs random access for range requests while embed.FS files
// are not seekable; bundles are small enough for this to be acceptable. A
// directory is reported as fs.ErrNotExist, so directory listings are impossible.
func readWebUIFile(files fs.FS, name string) (*bytes.Reader, fs.FileInfo, error) {
	file, err := files.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if stat.IsDir() {
		return nil, nil, fs.ErrNotExist
	}
	content, err := fs.ReadFile(files, name)
	if err != nil {
		return nil, nil, err
	}
	return bytes.NewReader(content), stat, nil
}

// routeApplication mounts the API and the UI on router. The generated server answers
// the /v1/ and /health/ paths it declares, and the same handler is additionally
// mounted under /api/ with that prefix stripped, because the UI fetches from there.
// The root pattern is registered last so the more specific handlers win; it serves the
// UI bundle when ui is non-nil and falls back to the API otherwise, so every request
// reaches a handler rather than a bare 404.
func routeApplication(router chi.Router, apiServer http.Handler, ui http.Handler) {
	router.Handle("/api/*", http.StripPrefix("/api", apiServer))
	router.Handle("/v1/*", apiServer)
	router.Handle("/health/*", apiServer)
	if ui != nil {
		router.Handle("/*", ui)
		return
	}
	router.Handle("/*", apiServer)
}

var _ http.Handler = webUIHandler{}
