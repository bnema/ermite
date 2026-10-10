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

// buildVersion is the module version stamped into the binary.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "(devel)"
	}
	return info.Main.Version
}

func version() string { return "ermite " + buildVersion() }
