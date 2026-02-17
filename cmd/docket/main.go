package main

import (
	"os"

	"github.com/ALT-F4-LLC/docket/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
