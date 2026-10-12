// SPDX-License-Identifier: AGPL-3.0-or-later

// Package app is the composition root and the command line of ermite: it
// parses the CLI, reads the environment, builds the adapters and wires them to
// the use cases. cmd/ermite is only its entry point.
package app

import (
	"runtime/debug"
)

// Run executes the ermite command line with args, the command-line arguments
// without the program name. It returns the error of the command, which the
// caller prints once.
func Run(args []string) error {
	root := newRootCmd()
	if args == nil {
		args = []string{}
	}
	root.SetArgs(args)
	return root.Execute()
}

// ExitCode is the process exit status for the error Run returned.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	return 1
}

// stamped is the release version set at link time by the image build
// (-ldflags "-X bnema.dev/ermite/internal/app.stamped=v0.1.0"). Empty for a
// plain go build, which then reports the module version recorded by the Go
// toolchain.
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
