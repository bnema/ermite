// SPDX-License-Identifier: AGPL-3.0-or-later

package githttp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteHookScripts writes the pre-receive and post-receive scripts into dir
// (created if missing). Each is a /bin/sh script, mode 0755, that execs the
// ermite binary at exe with `hook <name>`; exe is quoted for the shell. The
// files are replaced atomically.
func WriteHookScripts(dir, exe string) error {
	if !filepath.IsAbs(exe) {
		return fmt.Errorf("hook executable %q is not an absolute path", exe)
	}
	if strings.ContainsRune(exe, 0) || strings.ContainsAny(exe, "\n\r") {
		return fmt.Errorf("hook executable path %q has control characters", exe)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create hooks directory: %w", err)
	}
	for _, hook := range []string{HookPreReceive, HookPostReceive} {
		if err := writeExecutable(filepath.Join(dir, hook), hookScript(exe, hook)); err != nil {
			return fmt.Errorf("write %s hook: %w", hook, err)
		}
	}
	return nil
}

// hookScript returns the text of a hook script.
func hookScript(exe, hook string) string {
	return "#!/bin/sh\n# Written by ermite at start. Do not edit.\nexec " + shellQuote(exe) + " hook " + hook + "\n"
}

// shellQuote quotes s for /bin/sh with single quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func writeExecutable(path, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hook-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after the rename
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
