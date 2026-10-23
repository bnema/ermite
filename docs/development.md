# Development

## Build and test

```sh
go build ./cmd/ermite
go test ./...
```

`go test ./...` includes the end-to-end tests of `internal/app` (a real server per test). Run them before opening a pull request: CI runs them on `main` only.

Stamp the version with `-ldflags "-X bnema.dev/ermite/internal/app.stamped=<version>"`.

## Dev server

```sh
docker compose up        # http://localhost:7341
docker compose down -v   # also removes data and caches
```

The source is mounted and [air](https://github.com/air-verse/air) rebuilds on each change to `.go`, `.templ` or web assets. The admin token is in `compose.yaml`. The container runs as root, which maps to your user under rootless Docker.

## Docker image

```sh
docker build --build-arg VERSION="$(git describe --tags --always --dirty)" -t ermite .
```

Alpine with git, runs as uid 10001, data in `/data`. Behind a private route, `--build-arg GIT_REWRITE_BASE=<url>` rewrites the git host of the `bnema.dev/ermite-*` modules.

## Release

Pushing a `v*` tag runs `.github/workflows/release.yml` on ermite. It first runs the checks of `ci.yml`, end-to-end tests included, on the tagged commit. Then GoReleaser builds static Linux amd64 and arm64 binaries, smoke-tests the amd64 one and publishes both on the ermite release of the tag, with `checksums.txt` and a changelog. A tag with a suffix (`v1.2.0-rc.1`) is a prerelease. The GitHub mirror publishes nothing.

Check the configuration locally: `goreleaser check && goreleaser release --snapshot --clean`.

## Tools

Pinned in `go.mod`:

```sh
go tool buf lint
go tool buf generate        # proto/ -> gen/
go generate ./...           # mockery mocks of the ports
go tool hexcheck -fail-on warn ./...
```

## Architecture

```
cmd/ermite → internal/app → internal/usecase → internal/ports → internal/domain
                         ↘ internal/adapters (Git, KV, HTTP, API, web, client)
```

- `internal/app`: CLI and composition root.
- `internal/usecase` imports only `domain` and `ports`.
- Adapters import `domain`, `ports` and `internal/clientlimit`; never a use case, `app` or another adapter.
- The web adapter calls use cases through `ports`, never the generated API code.
- `internal/ports` imports only `domain`; `domain` imports nothing from the module.
- `internal/config` is used by `internal/app` only.

`internal/boundary/boundary_test.go` enforces the imports; `hexcheck` (`.hexcheck.yaml`) checks the layering.
