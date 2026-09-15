// Command aimesh runs provider-diverse, governed AI reviews (`aimesh review`) and explorations
// (`aimesh explore`).
package main

import (
	"os"

	"github.com/Tim-Butterfield/aimesh/internal/cli"
)

func main() { os.Exit(cli.Main()) }
