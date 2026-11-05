// Package web embeds the dashboard assets into the binary.
package web

import "embed"

//go:embed templates static
var FS embed.FS

// FS is the embedded dashboard filesystem (templates/index.html, static/*).
