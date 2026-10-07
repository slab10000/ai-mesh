package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/slab10000/ai-mesh/internal/mesh"
)

func main() {
	if err := mesh.Main(os.Args[1:]); err != nil {
		var exit *mesh.InteractiveExit
		if errors.As(err, &exit) {
			os.Exit(exit.Code)
		}
		fmt.Fprintln(os.Stderr, "mesh:", err)
		os.Exit(1)
	}
}
