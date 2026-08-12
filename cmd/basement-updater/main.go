package main

import (
	"context"
	"fmt"
	"os"

	"github.com/punkjazz-labs/basement/internal/update"
)

// version is stamped by the release workflow with -X main.version, exactly as
// the manager is. Before protocol 2 nothing on a machine could say which
// helper build was installed, which is why the swap this binary now performs
// needed a name to report in the first place.
var version = "dev"

func main() {
	if len(os.Args) != 2 {
		usage()
	}
	switch os.Args[1] {
	case "apply":
		apply()
	case "version":
		// Reporting takes no lock, writes nothing, reads no state directory
		// and needs no privilege. It stays runnable while a transaction
		// holds the apply lock, which is when someone is most likely to ask.
		identity, err := update.RunningHelperIdentity(version)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(update.HelperVersionLine(identity))
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: basement-updater apply|version")
	os.Exit(2)
}

func apply() {
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
