// SPDX-License-Identifier: AGPL-3.0-or-later

// Command ermite is a single-user Git forge.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(version())
		return
	}
	fmt.Fprintln(os.Stderr, "usage: ermite version")
	os.Exit(2)
}

func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "ermite (devel)"
	}
	return "ermite " + info.Main.Version
}
