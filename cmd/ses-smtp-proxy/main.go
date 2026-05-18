// Command ses-smtp-proxy bridges Amazon SES and an on-premise Microsoft
// Exchange server. See README.md for the high-level architecture.
package main

import (
	"fmt"
	"os"
)

// version is overwritten at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "ses-smtp-proxy: %v\n", err)
		os.Exit(1)
	}
}
