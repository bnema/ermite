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

// stamped is the release version set at link time by the image build
// (-ldflags "-X main.stamped=v0.1.0"). Empty for a plain go build, which then
// reports the module version recorded by the Go toolchain.
var stamped string

// buildVersion is the version stamped into the binary: the link-time version
// when set, else the module version of the build.
func buildVersion() string {
	if stamped != "" {
		return stamped
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "(devel)"
	}
	return info.Main.Version
}

func version() string { return "ermite " + buildVersion() }
