package main

import (
	"os"

	"github.com/wcpe/jrp/apps/jrpc/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
