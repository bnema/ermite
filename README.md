# ermite

A single-user Git forge in one Go binary. ermite hosts bare repositories, serves clone and push over HTTPS, and shows them in a read-only web interface.

Status: early development. `ermite serve` serves Git over HTTP (clone, fetch, push) and answers `GET /-/health`. There is no web interface or token command yet.

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
| `ERMITE_SOURCE_URL` | `$ERMITE_PUBLIC_URL/ermite` | link to the source (AGPL) |
| `ERMITE_LOG_LEVEL` | `info` | `trace`, `debug`, `info`, `warn`, `error` |
| `ERMITE_LOG_FORMAT` | `json` | `json` or `console` |

`ermite version` prints the version (and the server's when `ERMITE_URL` is set).

## Remote CLI

The same binary drives a running server over its API. It reads `ERMITE_URL` (the server address, `http://` or `https://`) and `ERMITE_TOKEN` (a bearer token; optional for public reads). A token sent over plain `http://` to a host that is not loopback triggers a warning on every call.

```sh
export ERMITE_URL=https://git.example.com ERMITE_TOKEN=ermite_...
ermite repo list | show <name> | create <name> [--description …] [--visibility public] | edit <name> [--description …] [--visibility …] [--default-branch …] | delete <name> [--yes]
ermite token create <label> [--preset cli|agent|ci] [--scope repo:read …] [--repo name …] [--expires 30d]   # prints the secret once
ermite token list | revoke <id>
ermite version                  # client and server, warns about missing server features
ermite watch [repo] [--json]    # follows events, reconnects and resumes
```

`--json` on `repo list|show|create|edit`, `token create|list` and `version` prints stable field names, listed in each command's `--help`; `watch --json` prints one object per line. Errors are one line on standard error (`ermite: …`) and exit status 1. `ermite completion bash|zsh|fish` prints a shell completion script that completes repository names from the server.

## Git over HTTP

```sh
git clone http://git.example.com/<repo>.git          # public repositories need no credentials
git push http://u:<token>@git.example.com/<repo>.git main
```

Use the admin token (`ERMITE_ADMIN_TOKEN`) or a stored token as the password; the username is ignored. Store it in a Git credential helper rather than in the URL. Pushing to a new name creates a private repository. Repository names are lowercase, `[a-z0-9._-]`. TLS is terminated by your proxy; list its network in `ERMITE_TRUSTED_PROXIES` so that failed-login limits see the real client address (`X-Forwarded-For`).

The server keeps its runtime files in `$ERMITE_DATA_DIR/run/` (the hook socket and scripts that git calls on each push); the hook socket path must stay under about 100 bytes.

## Run with Docker

```sh
docker build --build-arg VERSION="$(git describe --tags --always --dirty)" -t ermite .
docker run -d -p 7341:7341 -v ermite-data:/data \
  -e ERMITE_PUBLIC_URL=http://localhost:7341 -e ERMITE_ADMIN_TOKEN="$(head -c 32 /dev/urandom | base64)" ermite
```

The image is alpine with git, runs as uid 10001 and keeps its data in the `/data` volume. Logs are JSON by default. The build fetches `bnema.dev/ermite-*` from `git.bnema.dev`, so the host must reach it (NetBird); the build container uses the host's DNS and routes. Behind another route, pass `--build-arg GIT_REWRITE_BASE=http://git.bnema.dev:8088/` (and `--add-host`), as the CI job does.

## Develop with compose

```sh
docker compose up
```

Serves on `http://localhost:7341` with the development admin token in `compose.yaml`. The source is mounted and [air](https://github.com/air-verse/air) (`go tool air`, `.air.toml`) rebuilds and restarts the server on each change to a `.go` or `.templ` file; the module and build caches and the data live in named volumes. `docker compose down -v` removes them.

## Build

```sh
go build ./cmd/ermite
go test ./...
```

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
