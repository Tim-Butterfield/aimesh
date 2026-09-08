// Command aimesh is the single entry point: provider-diverse, governed AI review and exploration.
// It replaces the separate reviewmesh and exploremesh binaries — those domains are now reached as
// `aimesh review …` and `aimesh explore …`.
package main

import (
	"os"

	"github.com/Tim-Butterfield/aimesh/internal/cli"
)

func main() { os.Exit(cli.Main()) }
