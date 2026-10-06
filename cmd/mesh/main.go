package main

import (
	"fmt"
	"os"

	"github.com/slab10000/ai-mesh/internal/mesh"
)

func main() {
	if err := mesh.Main(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mesh:", err)
		os.Exit(1)
	}
}
