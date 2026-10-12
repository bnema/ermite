# ermite

A single-user Git forge in one Go binary. ermite hosts bare repositories, serves clone and push over HTTPS, reviews branches as merge requests, and shows everything in a web interface with live updates.

Status: early development. `ermite serve` serves Git over HTTP (clone, fetch, push), the API, `GET /-/health`, and the web: repositories, tree, files, commits, refs and merge requests. Visitors read public repositories; the owner signs in with a one-time link from the CLI to see private ones and to review and merge.

## Run

```sh
export ERMITE_DATA_DIR=/var/lib/ermite
export ERMITE_PUBLIC_URL=https://git.example.com
export ERMITE_ADMIN_TOKEN=$(head -c 32 /dev/urandom | base64)
ermite serve
```

| Variable | Default | Notes |
|---|---|---|
| `ERMITE_DATA_DIR` | required | absolute path of the data root |
| `ERMITE_PUBLIC_URL` | required | `http(s)` base URL, no path |
| `ERMITE_ADMIN_TOKEN` | required | at least 32 characters |
| `ERMITE_ADDR` | `:7341` | listen address |
| `ERMITE_TRUSTED_PROXIES` | empty | comma-separated CIDRs |
| `ERMITE_GIT` | `git` | git 2.45 or newer |
| `ERMITE_GIT_READER` | `ermite-git` | `ermite-git` reads refs, trees, blobs and history with the [ermite-git](https://git.bnema.dev/bnema/ermite-git) library where it is faster than the `git` binary and falls back to `git` elsewhere and for repositories it does not read; `cli` uses the `git` binary for every read (the escape hatch if a read looks wrong: set it and restart) |
| `ERMITE_SOURCE_URL` | `$ERMITE_PUBLIC_URL/ermite` | link to the source (AGPL) |
| `ERMITE_LOG_LEVEL` | `info` | `trace`, `debug`, `info`, `warn`, `error` |
| `ERMITE_LOG_FORMAT` | `json` | `json` or `console` |
| `ERMITE_COMMITTER_NAME` | `ermite` | committer of squash merges |
| `ERMITE_COMMITTER_EMAIL` | `ermite@<public host>` | committer email of squash merges |

`ermite version` prints the version (and the server's when `ERMITE_URL` is set).

## Remote CLI

The same binary drives a running server over its API. It reads `ERMITE_URL` (the server address, `http://` or `https://`) and `ERMITE_TOKEN` (a bearer token; optional for public reads). A token sent over plain `http://` to a host that is not loopback triggers a warning on every call.

```sh
export ERMITE_URL=https://git.example.com ERMITE_TOKEN=ermite_...
ermite repo list | show <name> | create <name> [--description …] [--visibility public] | edit <name> [--description …] [--visibility …] [--default-branch …] | delete <name> [--yes]
ermite token create <label> [--preset cli|agent|ci] [--scope repo:read …] [--repo name …] [--expires 30d]   # prints the secret once
ermite token list | revoke <id>
ermite mr create <repo> --source <branch> --title … | list <repo> | show <repo> <n> [--diff]
ermite mr comment <repo> <n> -m … [--path … --line … --side old|new] | resolve <repo> <n> <comment>
ermite mr merge <repo> <n> --head <id> [--squash] [--delete-branch] | close <repo> <n>
ermite web login                # prints a one-time sign-in link (60 s)
ermite web logout --all         # signs every browser out
ermite signing-key show         # public key and allowed_signers line of squash commits
ermite version                  # client and server, warns about missing server features
ermite watch [repo] [--json]    # follows events, reconnects and resumes
```

An agent skill describing the CLI and the forge lives in [`skills/ermite`](skills/ermite/SKILL.md); link or copy that folder into your agent's skills directory.

`--json` on `repo list|show|create|edit`, `token create|list` and `version` prints stable field names, listed in each command's `--help`; `watch --json` prints one object per line. Errors are one line on standard error (`ermite: …`) and exit status 1. `ermite completion bash|zsh|fish` prints a shell completion script that completes repository names from the server.

## Git over HTTP

```sh
git clone http://git.example.com/<repo>.git          # public repositories need no credentials
git push http://u:<token>@git.example.com/<repo>.git main
```

Use the admin token (`ERMITE_ADMIN_TOKEN`) or a stored token as the password; the username is ignored. Store it in a Git credential helper rather than in the URL. Pushing to a new name creates a private repository. Repository names are lowercase, `[a-z0-9._-]`. TLS is terminated by your proxy; list its network in `ERMITE_TRUSTED_PROXIES` so that failed-login limits see the real client address (`X-Forwarded-For`).

## Merge requests

A merge request proposes a branch for its target (the default branch unless set). The merge is refused when the branch moved since the reviewed head (`--head`) or when the target is not an ancestor of it. Fast-forward moves the target; squash writes one commit signed by the server's SSH key (`ermite signing-key show` prints the `allowed_signers` line for `git verify-commit`). `ermite repo edit --merge-modes` limits the modes per repository. `--delete-branch` (a checkbox on the web) deletes the source branch after the merge, unless it is the default branch or another open request uses it. Tokens need `mr:write` to open and comment, and `mr:merge` to merge (and delete the merged branch).

## Web sign-in

`ermite web login`, run with the admin token or a token with every scope, prints a link that works once within 60 seconds. Opening it and confirming sets a session cookie for 12 hours (never longer than the token that asked for the link). It needs `https` in `ERMITE_PUBLIC_URL` (except on localhost). Browser writes require the session and an `Origin` equal to `ERMITE_PUBLIC_URL`. `ermite web logout --all`, or revoking a token with every scope, ends every session.

The server keeps its runtime files in `$ERMITE_DATA_DIR/run/` (the hook socket and scripts that git calls on each push); the hook socket path must stay under about 100 bytes.

## Run with Docker

```sh
docker build --build-arg VERSION="$(git describe --tags --always --dirty)" -t ermite .
token=$(head -c 32 /dev/urandom | base64)   # keep it: it is the root credential
docker run -d -p 127.0.0.1:7341:7341 -v ermite-data:/data \
  -e ERMITE_PUBLIC_URL=http://localhost:7341 -e ERMITE_ADMIN_TOKEN="$token" ermite
```

The image is alpine with git, runs as uid 10001 and keeps its data in the `/data` volume. Logs are JSON by default. The build fetches `bnema.dev/ermite-*` from `git.bnema.dev`, so the host must reach it (NetBird); the build container uses the host's DNS and routes. Behind another route, pass `--build-arg GIT_REWRITE_BASE=http://git.bnema.dev:8088/` with `--network host`, as the CI job does.

## Develop with compose

```sh
docker compose up
```

Serves on `http://localhost:7341` with the development admin token in `compose.yaml`. The source is mounted and [air](https://github.com/air-verse/air) (`go tool air`, `.air.toml`) rebuilds and restarts the server on each change to a `.go` or `.templ` file or an embedded web asset; the module and build caches and the data live in named volumes. `docker compose down -v` removes them. The dev container runs as root, which maps to your user under rootless Docker.

## Build

```sh
go build ./cmd/ermite
go test ./...
```

## Architecture

`cmd/ermite` (entry point, calls `app.Run`) → `internal/app` (CLI and composition root: builds the adapters and wires them to the use cases) → `internal/usecase` (use cases), `internal/ports` (interfaces and parameter types that cross layers), `internal/domain` (pure values and rules) and `internal/adapters` (Git, KV store, HTTP, API, web, client implementations of the ports).

- `internal/usecase` imports only `internal/domain` and `internal/ports`.
- Adapters import `internal/domain`, `internal/ports` and the shared `internal/clientlimit`; never a use case, `internal/app` or another adapter.
- `internal/ports` imports only `internal/domain`; `internal/domain` imports nothing from the module.
- `internal/config` (environment parsing) is used by `internal/app` only.

`internal/boundary/boundary_test.go` enforces these import rules on production files; `hexcheck` (`.hexcheck.yaml`) checks the layering of types and behaviour. The release version is stamped with `-ldflags "-X bnema.dev/ermite/internal/app.stamped=<version>"`.

## Develop

Tools are pinned in `go.mod`:

```sh
go tool buf lint
go tool buf generate        # proto/ -> gen/
go generate ./...           # mockery mocks of the ports
go tool hexcheck -fail-on warn ./...
```

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE).
