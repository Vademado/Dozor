// Package web embeds the entire standalone frontend.
package web

import "embed"

//go:embed index.html assets
var Files embed.FS
