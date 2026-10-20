// Command pkgdepot builds and deploys offline private npm / PyPI registries.
package main

import (
	"os"

	"github.com/SparkElf/pkgdepot/internal/cli"
)

func main() {
	app := cli.New(os.Stdout, os.Stderr)
	os.Exit(app.Run(os.Args[1:]))
}
