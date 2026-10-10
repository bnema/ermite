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

`ermite version` prints the version.

## Git over HTTP

```sh
git clone http://git.example.com/<repo>.git          # public repositories need no credentials
git push http://u:<token>@git.example.com/<repo>.git main
```

Use the admin token (`ERMITE_ADMIN_TOKEN`) or a stored token as the password; the username is ignored. Store it in a Git credential helper rather than in the URL. Pushing to a new name creates a private repository. Repository names are lowercase, `[a-z0-9._-]`. TLS is terminated by your proxy; list its network in `ERMITE_TRUSTED_PROXIES` so that failed-login limits see the real client address (`X-Forwarded-For`).

The server keeps its runtime files in `$ERMITE_DATA_DIR/run/` (the hook socket and scripts that git calls on each push); the hook socket path must stay under about 100 bytes.

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
