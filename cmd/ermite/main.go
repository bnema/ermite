// SPDX-License-Identifier: AGPL-3.0-or-later

// Command ermite is a single-user Git forge.
package main

import (
	"fmt"
	"os"

	"bnema.dev/ermite/internal/app"
)

func main() {
	if err := app.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ermite:", err)
		os.Exit(app.ExitCode(err))
	}
}
