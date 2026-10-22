# Releases

A release binds a Git tag to a title, Markdown notes and named assets. There is
one release per tag. Published releases follow repository visibility; drafts
are visible only to writers. The web Releases tab is read-only.

```sh
ermite release create app v1.0.0 --target main --title 'Version 1' --notes-file notes.md --asset dist/app
ermite release list app --json
ermite release view app latest
ermite release edit app v1.0.0 --notes 'Updated notes'
ermite release upload app v1.0.0 dist/app --clobber
ermite release download app latest --pattern 'app*' --dir downloads
ermite release delete app v1.0.0 --yes
```

Create/edit accept `--draft`, `--prerelease`, `--latest`, `--target` and notes.
Use `edit --publish` to publish a draft, `--no-prerelease` to clear that flag.
An existing tag never moves; `--target` creates a missing tag. A draft can name a
missing tag. Deleting a tag turns its release into a draft and keeps its assets;
recreating the tag does not republish it. Deleting a release does not delete the tag.

`latest` is the newest published non-prerelease by creation time and id, unless
pinned with `--latest` / REST `make_latest: "true"`. Drafting or deleting the
pinned release clears the pin. Asset names are unique within a release, 1–255
characters, without slashes, backslashes or control characters. CLI `--clobber`
replaces a same-named asset; REST publishers delete it then upload again.

Writes require `release:write` (included in the `cli` preset), or a CI job token
with `permissions: contents: write` for that repository. Branch-limited tokens
cannot write releases. Read access to a private repository needs `repo:read`;
unreadable releases, including drafts, answer not found. Older servers without
the `releases` feature are refused by the CLI.

## Limits and storage

| Variable | Default |
|---|---|
| `ERMITE_RELEASE_ASSET_MAX` | `2GiB` per asset |
| `ERMITE_RELEASE_ASSETS_MAX` | `1000` per release |
| `ERMITE_RELEASE_QUOTA` | `20GiB` per repository |

Quota includes uploads in progress. Exceeding limits gives REST 422 or RPC
`resource_exhausted`. Assets use the shared [blob store](storage.md), disk by
default or S3. Downloads pass through ermite, never redirect to the bucket, and
support single byte ranges with 206, Content-Length and Content-Disposition.
Stable links:

- `/<repo>/releases/download/<tag>/<asset>`
- `/<repo>/releases/latest/download/<asset>`

## GitHub-compatible publishers

REST is at `<public URL>/-/github/api`, uploads at
`<public URL>/-/github/api/uploads`. Use the external `ERMITE_PUBLIC_URL`, not
the S3 endpoint. Stored-token publishers need `release:write` and, for private
repository reads, `repo:read`. The owner component is `ERMITE_OWNER` (`owner` by
default).

GoReleaser does not read `GITHUB_API_URL` or `GITHUB_SERVER_URL` by itself, so
template `github_urls` from them (replace the fallback host and repository):

```yaml
version: 2
# In CI the runner sets GITHUB_API_URL and GITHUB_SERVER_URL to the address the
# job can reach; the public URLs are the fallback for a local run.
github_urls:
  api: '{{ or (index .Env "GITHUB_API_URL") "https://git.example.org/-/github/api" }}/'
  upload: '{{ or (index .Env "GITHUB_API_URL") "https://git.example.org/-/github/api" }}/uploads/'
  download: '{{ or (index .Env "GITHUB_SERVER_URL") "https://git.example.org" }}/'
release:
  github:
    owner: owner
    name: app
```

In a CI job the runner rewrites the server and API URLs to an origin the job
can reach, which may differ from the public URL (for example `http://ermite:7341`
with a relayed forge). Sandboxes block private and CGNAT ranges, so a job that
targets only the public URL times out (`dial tcp 100.x:443: i/o timeout`). A
plain-http relay needs no TLS option. The API base gets `api/v3/` appended by
the client; ermite accepts that alias. Locally, run
`GITHUB_TOKEN=<token> goreleaser release --clean` from a tagged checkout; the
fallbacks apply.

Workflow:

```yaml
name: release
on:
  push:
    tags: ['v*']
permissions:
  contents: write
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - uses: goreleaser/goreleaser-action@v6
        with:
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

An existing GitHub workflow must be enabled on ermite:
`ermite workflow enable <repo> .github/workflows/release.yml`.

On an ermite runner, the job supplies `GITHUB_API_URL`, `GITHUB_SERVER_URL` and
`GITHUB_TOKEN`; no endpoint override is needed for action-gh-release:

```yaml
name: release
on:
  push:
    tags: ['v*']
permissions:
  contents: write
jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: mkdir -p dist && printf 'hello\n' > dist/app.txt
      - uses: softprops/action-gh-release@v2
        with:
          files: dist/*
          body: Release built by CI
```

`contents: read` is insufficient and gets 403. Automatic release-note generation,
discussions, reactions, immutability and attestations are not supported.
