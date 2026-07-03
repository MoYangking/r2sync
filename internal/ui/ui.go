package ui

import (
	"embed"
	"io/fs"
)

//go:embed static/*
var static embed.FS

func StaticFS() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		return static
	}
	return sub
}
