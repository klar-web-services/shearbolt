// Command shearbolt is a deployment gate for known vulnerabilities.
package main

import (
	"os"

	"github.com/klar-web-services/shearbolt/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
