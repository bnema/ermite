// SPDX-License-Identifier: AGPL-3.0-or-later

// Command ermite is a single-user Git forge.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "ermite:", err)
		os.Exit(1)
	}
}

func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "ermite (devel)"
	}
	return "ermite " + info.Main.Version
}
