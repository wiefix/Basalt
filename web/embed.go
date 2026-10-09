// Package web holds the compiled web UI (web/dist, built with `make web`).
package web

import "embed"

// Dist is the Vite build output. dist/.keep keeps the directory present so
// the binary builds even when the UI has not been built.
//
//go:embed all:dist
var Dist embed.FS
