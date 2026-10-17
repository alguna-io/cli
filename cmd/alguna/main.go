// Command alguna is the command-line client for the Alguna API.
package main

import (
	_ "embed"

	"github.com/alguna-io/cli"
)

// manifest is the command manifest, written from the API spec when the CLI is generated.
//
//go:embed manifest.json
var manifest []byte

func main() {
	cli.Main(manifest)
}
