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
| `ERMITE_VIDEO_HOSTS` | `github.com,user-images.githubusercontent.com,private-user-images.githubusercontent.com,github-production-user-asset-6210df.s3.amazonaws.com` | comma-separated exact external video hostnames, replacing the defaults; no URLs, wildcards or ports. The S3 host receives GitHub user-attachment redirects. |
| `ERMITE_ROBOTS_TXT_FILE` | empty | file served as `/robots.txt` instead of the default, read once at startup. A missing, unreadable, empty or over 500 KiB file stops startup. See below |
| `ERMITE_LOG_LEVEL` | `info` | `trace`, `debug`, `info`, `warn`, `error` |
| `ERMITE_LOG_FORMAT` | `json` | `json` or `console` |

### `/robots.txt`

The default lets crawlers index the home page, repository summaries, issues, merge requests and releases. It keeps them off the views with no end or a high cost: files, directories, raw files, commits, history, compare, refs, merge request file diffs, the query variants of a merge request page (`?view=`, `?from=`, `?on=`), CI, release downloads, Git transfers and everything under `/-/` except static assets. It also asks for `Crawl-delay: 5`. Fetch `/robots.txt` on your instance to see it.

`robots.txt` is a reserved repository name, so no repository hides the file. It is served with `Cache-Control: public, max-age=3600`.

## Git

| Variable | Default | Notes |
|---|---|---|
| `ERMITE_GIT` | `git` | git 2.45 or newer |
| `ERMITE_GIT_READER` | `ermite-git` | `ermite-git` reads with the [ermite-git](https://github.com/bnema/ermite-git) library where it is faster, and falls back to `git`. `cli` uses `git` for every read: set it and restart if a read looks wrong |
| `ERMITE_COMMITTER_NAME` | `ermite` | committer of squash and merge commits |
| `ERMITE_COMMITTER_EMAIL` | `ermite@<public host>` | committer email of squash and merge commits |

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

## Blob storage and releases

| Variable | Default | Notes |
|---|---|---|
| `ERMITE_S3_BUCKET` | empty | selects S3 when nonempty; otherwise disk under `$ERMITE_DATA_DIR/blobs` |
| `ERMITE_S3_ENDPOINT` | empty | http(s) endpoint; empty uses AWS region resolution; no credentials, path, query or fragment |
| `ERMITE_S3_REGION` | empty | required with a bucket, including custom endpoints |
| `ERMITE_S3_ACCESS_KEY_ID` | empty | explicit key, required with a bucket |
| `ERMITE_S3_SECRET_ACCESS_KEY` | empty | explicit secret, required with a bucket |
| `ERMITE_S3_FORCE_PATH_STYLE` | `0` | `0`/`1` or `false`/`true`; `1` for MinIO without wildcard DNS |
| `ERMITE_S3_PREFIX` | empty | ASCII key prefix, normalized to a trailing `/`; no empty, `.` or `..` segments |
| `ERMITE_RELEASE_ASSET_MAX` | `2GiB` | one release asset |
| `ERMITE_RELEASE_ASSETS_MAX` | `1000` | positive integer, per release |
| `ERMITE_RELEASE_QUOTA` | `20GiB` | per repository, including uploads in progress |

Sizes accept bytes or `KiB`, `MiB`, `GiB`, `TiB`, from 1 MiB to 1 PiB.
See [storage](storage.md) for AWS, Scaleway, R2, MinIO, permissions and backups,
and [releases](releases.md) for publishing.

### Secret files

`ERMITE_ADMIN_TOKEN`, `ERMITE_SECRETS_KEY`, `ERMITE_S3_ACCESS_KEY_ID` and
`ERMITE_S3_SECRET_ACCESS_KEY` each support a `_FILE` variant, for example
`ERMITE_ADMIN_TOKEN_FILE=/run/secrets/admin-token`. Set either the direct value
or its file, never both. Files are read once at startup; trailing CR/LF is
removed. Empty or unreadable files fail validation. Secret contents and file
paths are not included in file-resolution errors. No AWS credential chain is used.

## Runtime files

The hook socket and scripts git calls on each push live in `$ERMITE_DATA_DIR/run/`. The socket path must stay under about 100 bytes, so keep `ERMITE_DATA_DIR` short.

## Storage schema

This build uses storage schema 6. Once it has run on your data, an older ermite refuses to start. To downgrade, restore the backup taken before the upgrade.

## README media

Trusted Markdown renders video images (`![label](clip.mp4)`, also `.webm`, `.mov`,
`.m4v` and `.ogv`) as native players. Repository-relative destinations use the raw
route of the viewed ref; external HTTP(S) destinations require an
`ERMITE_VIDEO_HOSTS` host. A standalone link or autolink to an allowed host
renders as a player only when its path has a supported video extension or is an
extensionless GitHub `github.com/user-attachments/assets/<id>` URL. Repository,
issue and other ordinary URLs stay links.
Other images show their alt text; other links keep the existing link policy.
Raw HTML stays disabled. Untrusted public issue Markdown never loads videos.
Raw video types are detected by bytes, not extensions. Seeking supports single
byte ranges; it streams and discards the preceding bytes, so small clips work
best. Multipart ranges are ignored.
