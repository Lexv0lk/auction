// Package web embeds templates and static files into the application binary.
package web

import "embed"

// Files contains the application templates and static assets.
//
//go:embed templates/*.gohtml static/*
var Files embed.FS
