// Package buildinfo holds values stamped into the binary at release time.
package buildinfo

// Version is the Shearbolt version. Release builds set it with
// -ldflags "-X github.com/klar-web-services/shearbolt/internal/buildinfo.Version=1.2.3".
var Version = "dev"
