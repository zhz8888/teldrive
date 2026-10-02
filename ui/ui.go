// Package ui embeds the compiled single-page application so the server can
// serve the web interface without a separate asset directory or a CDN.
package ui

import (
	"embed"
)

// StaticFS holds the built UI bundle (ui/dist) under the same paths the Vite
// build produced, so the HTTP layer can serve files such as "dist/index.html".
//
//go:embed all:dist
var StaticFS embed.FS
