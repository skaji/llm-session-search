package main

import (
	"fmt"
	"os"

	"github.com/skaji/llm-session-search/internal/search"
)

var version = "dev"

func main() {
	if err := search.Run(os.Args[1:], version, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "llm-session-search:", err)
		os.Exit(1)
	}
}
