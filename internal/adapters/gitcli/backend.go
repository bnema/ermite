// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/ports"
)

var _ ports.GitBackend = (*Git)(nil)

// backendOwner is REMOTE_USER of a push: ermite authorizes before Git runs,
// so Git only needs to see a user.
const backendOwner = "owner"

// HooksDir is the directory, below the data root, that holds the hook
// scripts every receive-pack runs.
func HooksDir(dataDir string) string { return filepath.Join(dataDir, "run", "hooks") }

// HTTPBackend resolves git-http-backend through `git --exec-path` of the
// configured git (once), and builds its environment: the isolated one,
// GIT_PROJECT_ROOT=<data>/repos, GIT_HTTP_EXPORT_ALL and the configuration
// passed by GIT_CONFIG_COUNT/KEY/VALUE, so that nothing is written to the
// repositories:
//
//	core.hooksPath=<data>/run/hooks
//	receive.fsckObjects=true
//	uploadpack.allowFilter=true
//	http.receivepack=true            (push only)
//	receive.maxInputSize=<max>       (push only, when configured)
//
// A push also gets REMOTE_USER=owner.
func (g *Git) HTTPBackend(ctx context.Context, push bool) (ports.HTTPBackend, error) {
	path, err := g.backendPath(ctx)
	if err != nil {
		return ports.HTTPBackend{}, err
	}
	conf := [][2]string{
		{"core.hooksPath", HooksDir(g.dataDir)},
		{"receive.fsckObjects", "true"},
		{"uploadpack.allowFilter", "true"},
	}
	if push {
		conf = append(conf, [2]string{"http.receivepack", "true"})
		if g.maxRecv > 0 {
			conf = append(conf, [2]string{"receive.maxInputSize", strconv.FormatInt(g.maxRecv, 10)})
		}
	}
	env := append([]string(nil), g.env...)
	env = append(env,
		"GIT_PROJECT_ROOT="+g.reposDir(),
		"GIT_HTTP_EXPORT_ALL=1",
		"GIT_CONFIG_COUNT="+strconv.Itoa(len(conf)),
	)
	for i, kv := range conf {
		n := strconv.Itoa(i)
		env = append(env, "GIT_CONFIG_KEY_"+n+"="+kv[0], "GIT_CONFIG_VALUE_"+n+"="+kv[1])
	}
	if push {
		env = append(env, "REMOTE_USER="+backendOwner)
	}
	return ports.HTTPBackend{Path: path, Dir: g.reposDir(), Env: env}, nil
}

// backendPath returns <git --exec-path>/git-http-backend, cached after the
// first success.
func (g *Git) backendPath(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.backend != "" {
		return g.backend, nil
	}
	out, err := g.run(ctx, "--exec-path")
	if err != nil {
		return "", fmt.Errorf("find git exec path: %w", err)
	}
	execPath := strings.TrimSpace(out)
	if execPath == "" {
		return "", fmt.Errorf("find git exec path: git printed nothing")
	}
	path := filepath.Join(execPath, "git-http-backend")
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("find git-http-backend: %w", err)
	}
	if fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("find git-http-backend: %s is not an executable", path)
	}
	g.backend = path
	return path, nil
}
