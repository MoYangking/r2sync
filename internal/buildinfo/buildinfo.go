// Package buildinfo carries build-time version metadata injected via
// -ldflags "-X r2sync/internal/buildinfo.Version=v1.2.3".
package buildinfo

var Version = "dev"
