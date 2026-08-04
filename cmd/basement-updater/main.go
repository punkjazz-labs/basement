package main

import (
	"context"
	"fmt"
	"os"

	"github.com/punkjazz-labs/basement/internal/update"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "apply" {
		fmt.Fprintln(os.Stderr, "usage: basement-updater apply")
		os.Exit(2)
	}
	keys, err := update.ProductionKeyRing()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := update.NewSystemUpdater(keys).Apply(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
