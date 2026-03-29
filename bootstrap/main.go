package main

import (
	"os"

	"github.com/jdwlabs/infrastructure/bootstrap/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
