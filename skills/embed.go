// Package skills embeds the coding-agent skill that `shearbolt skill install` writes.
package skills

import "embed"

// Files holds the agent skill; paths start with "shearbolt/".
//
//go:embed shearbolt
var Files embed.FS
