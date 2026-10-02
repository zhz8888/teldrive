package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestWebUIHandlerServesAssetsAndHistoryFallback(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<main>Teldrive UI</main>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "app-deadbeef.js"), []byte("console.log('ok')"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A stable name that happens to contain hyphens, a hashed name whose hash contains
	// one, and the copied pdfjs tree: only the real build artefact may be immutable.
	for _, name := range []string{"logo-dark.svg", "epub-p5nIh-Lz.js", "index-B359l0XO.css"} {
		if err := os.WriteFile(filepath.Join(root, "assets", name), []byte("asset"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "images", "favicon-16x16.png"), []byte("icon"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := webUIHandler{files: os.DirFS(root)}

	for _, test := range []struct {
		path        string
		status      int
		body        string
		cachePrefix string
	}{
		{path: "/", status: http.StatusOK, body: "Teldrive UI", cachePrefix: "no-cache"},
		{path: "/files/deep/folder", status: http.StatusOK, body: "Teldrive UI", cachePrefix: "no-cache"},
		{path: "/assets/app-deadbeef.js", status: http.StatusOK, body: "console.log", cachePrefix: "public, max-age=31536000"},
		{path: "/assets/epub-p5nIh-Lz.js", status: http.StatusOK, body: "asset", cachePrefix: "public, max-age=31536000"},
		{path: "/assets/index-B359l0XO.css", status: http.StatusOK, body: "asset", cachePrefix: "public, max-age=31536000"},
		{path: "/assets/logo-dark.svg", status: http.StatusOK, body: "asset", cachePrefix: "public, max-age=3600"},
		{path: "/images/favicon-16x16.png", status: http.StatusOK, body: "icon", cachePrefix: "public, max-age=3600"},
		{path: "/assets/missing.js", status: http.StatusNotFound, body: "404 page not found"},
	} {
		t.Run(test.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			result := response.Result()
			defer result.Body.Close()
			content, _ := io.ReadAll(result.Body)
			if result.StatusCode != test.status || !strings.Contains(string(content), test.body) {
				t.Fatalf("GET %s = %d %q", test.path, result.StatusCode, content)
			}
			if test.cachePrefix != "" && !strings.HasPrefix(result.Header.Get("Cache-Control"), test.cachePrefix) {
				t.Fatalf("GET %s cache control = %q", test.path, result.Header.Get("Cache-Control"))
			}
			if test.status == http.StatusOK && !strings.Contains(result.Header.Get("Content-Security-Policy"), "script-src 'self'") {
				t.Fatalf("GET %s CSP = %q", test.path, result.Header.Get("Content-Security-Policy"))
			}
		})
	}
}

// TestIsHashedUIAsset pins the content-hash rule against the names the bundled UI
// actually uses: generated bundle files are immutable, while stable names that merely
// contain a hyphen, such as images/favicon-16x16.png and fonts/geist.woff2, must stay
// on the one-hour cache so a deployment cannot leave them stale.
func TestIsHashedUIAsset(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		want bool
	}{
		{name: "assets/index-DpMfofu4.js", want: true},
		{name: "assets/index-B359l0XO.css", want: true},
		{name: "assets/app-deadbeef.js", want: true},
		{name: "assets/epub-p5nIh-Lz.js", want: true},
		{name: "assets/pdf.worker.min-Dswkl-cV.mjs", want: true},
		{name: "assets/logo-dark.svg", want: false},
		{name: "assets/app-1.js", want: false},
		{name: "images/favicon-16x16.png", want: false},
		{name: "images/apple-touch-icon.png", want: false},
		{name: "fonts/geist.woff2", want: false},
		{name: "pdfjs/wasm/quickjs-eval.wasm", want: false},
		{name: "pdfjs/wasm/jbig2_nowasm_fallback.js", want: false},
		{name: "pdfjs/iccs/CGATS001Compat-v2-micro.icc", want: false},
		{name: "assets/nested/app-deadbeef.js", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isHashedUIAsset(test.name); got != test.want {
				t.Fatalf("isHashedUIAsset(%q) = %t, want %t", test.name, got, test.want)
			}
		})
	}
}

func TestWebUIHandlerUsesEmbeddedSPA(t *testing.T) {
	t.Parallel()
	if handler, err := newWebUIHandler(); err != nil || handler == nil {
		t.Fatalf("embedded UI = %T, %v", handler, err)
	}
}

func TestRouteApplicationKeepsAPIAndSPASeparate(t *testing.T) {
	t.Parallel()
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "api:"+r.URL.Path)
	})
	ui := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ui:"+r.URL.Path)
	})
	mux := chi.NewRouter()
	routeApplication(mux, api, ui)

	tests := map[string]string{
		"/api/v1/files": "api:/v1/files",
		"/v1/files":     "api:/v1/files",
		"/health/live":  "api:/health/live",
		"/files":        "ui:/files",
	}
	for requestPath, want := range tests {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, requestPath, nil))
		if got := strings.TrimSpace(response.Body.String()); got != want {
			t.Fatalf("GET %s = %q, want %q", requestPath, got, want)
		}
	}
}
