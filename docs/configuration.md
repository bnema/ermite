# Configuration

`ermite serve` reads its settings from the environment.

## Required

| Variable | Notes |
|---|---|
| `ERMITE_DATA_DIR` | absolute path of the data root |
| `ERMITE_PUBLIC_URL` | `http(s)` base URL, no path |
| `ERMITE_ADMIN_TOKEN` | at least 32 characters; the root credential |

```sh
export ERMITE_DATA_DIR=/var/lib/ermite
export ERMITE_PUBLIC_URL=https://git.example.com
export ERMITE_ADMIN_TOKEN=$(head -c 32 /dev/urandom | base64)
ermite serve
```

## Server

| Variable | Default | Notes |
|---|---|---|
| `ERMITE_ADDR` | `:7341` | listen address |
| `ERMITE_TRUSTED_PROXIES` | empty | comma-separated CIDRs of your TLS proxy, so login rate limits see the real client (`X-Forwarded-For`) |
| `ERMITE_OWNER` | `owner` | owner login: web header, passkey name, and `<owner>` in CI (`github.repository`). Same rules as repository names |
| `ERMITE_SOURCE_URL` | `$ERMITE_PUBLIC_URL/ermite` | link to the source (AGPL) |
| `ERMITE_LOG_LEVEL` | `info` | `trace`, `debug`, `info`, `warn`, `error` |
| `ERMITE_LOG_FORMAT` | `json` | `json` or `console` |

## Git

| Variable | Default | Notes |
|---|---|---|
| `ERMITE_GIT` | `git` | git 2.45 or newer |
| `ERMITE_GIT_READER` | `ermite-git` | `ermite-git` reads with the [ermite-git](https://github.com/bnema/ermite-git) library where it is faster, and falls back to `git`. `cli` uses `git` for every read: set it and restart if a read looks wrong |
| `ERMITE_COMMITTER_NAME` | `ermite` | committer of squash merges |
| `ERMITE_COMMITTER_EMAIL` | `ermite@<public host>` | committer email of squash merges |

## CI

| Variable | Default | Notes |
|---|---|---|
| `ERMITE_SECRETS_KEY` | empty | 32 random bytes in base64. Encrypts CI secrets and the OIDC key. See below |
| `ERMITE_CI_LOG_RETENTION` | `90d` | how long runs, jobs, checks and logs are kept. `Nd` or a Go duration (`720h`), at least `1h` |
| `ERMITE_CI_POLL_TIMEOUT` | `50s` | how long a runner's `FetchJob` call is held when no job is queued, `1s` to `55s`. Keep it below your proxy's response timeout (`25s` behind a proxy that cuts at 30 s) |
| `ERMITE_CI_ARTIFACTS` | `1` | `0` turns artifacts off |
| `ERMITE_CI_ARTIFACT_RETENTION` | `90d` | default and maximum `retention-days`, at least one day |
| `ERMITE_CI_ARTIFACT_QUOTA` | `10GiB` | artifact space per repository, uploads in progress included. Bytes or `KiB`, `MiB`, `GiB`, `TiB` |
| `ERMITE_CI_ARTIFACT_MAX` | `5GiB` | size of one artifact |

### `ERMITE_SECRETS_KEY`

```sh
head -c 32 /dev/urandom | base64
```

- Store it like the admin token.
- Without it, `ermite secret set` fails, jobs get no secrets and OIDC is off. Variables and environments still work.
- The server refuses to start with a different key than the one the secrets were sealed with.
- Losing the key loses every secret. A backup without the key reveals none.

## Runtime files

The hook socket and scripts git calls on each push live in `$ERMITE_DATA_DIR/run/`. The socket path must stay under about 100 bytes, so keep `ERMITE_DATA_DIR` short.

## Storage schema

This build uses storage schema 3. Once it has run on your data, an older ermite refuses to start. To downgrade, restore the backup taken before the upgrade.
