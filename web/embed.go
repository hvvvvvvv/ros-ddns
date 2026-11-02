// Package webui embeds the browser assets served by the HTTP API.
package webui

import "embed"

// FS contains index.html and the asset files referenced as /assets/<name>.
//
//go:embed index.html styles.css *.js
var FS embed.FS
