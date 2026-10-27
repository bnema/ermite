---
name: ermite
description: Explains how to use an ermite Git forge and its CLI - clone and push over HTTPS, repositories, tokens and scopes, merge requests (review, comment, merge), issues, CI runs, secrets, releases, mirrors, live events, and running the server. Use when the user mentions ermite, runs `ermite` commands, pushes to or reviews on an ermite server, or self-hosts one.
---

# ermite

A single-user Git forge in one binary. `ermite serve` runs the server; every other command is a client of its API. The owner holds the admin token and creates scoped tokens for agents and CI. Every command and flag: [CLI.md](CLI.md). `ermite <cmd> --help` is authoritative; JSON fields are in it or its parent's.

## Connect

```sh
export ERMITE_URL=https://git.example.com
export ERMITE_TOKEN=ermite_...   # optional for public reads
ermite version                   # client + server versions, warns about missing features
```

The owner may keep `url` and `token.cmd` (a command printing the token, e.g. `pass show ermite/token`) in `~/.config/ermite/config`. As an agent, use `ERMITE_TOKEN` or your own `ERMITE_CONFIG` file; never edit the owner's file or run its `token.cmd`.

Never print, log, commit or paste a token or a `web login` link. Prefer a credential helper to a token in a URL, and `https://` (plain `http://` to a remote host warns).

## Git

```sh
git clone https://git.example.com/<repo>.git        # public: no credentials
git push https://git.example.com/<repo>.git main    # password = token, username ignored
```

Pushing to a missing name creates a private repository (`repo:write`). Names: lowercase `[a-z0-9._-]`, 1-100 chars. Private repos need `repo:read`. A token with `--branch-prefix agent/x/` may push only `refs/heads/agent/x/…`: work there and open a merge request.

## Review a merge request

1. `ermite mr list <repo> --json`
2. `ermite mr show <repo> <n> --diff --json`: keep `head_id`, take line numbers from the hunks.
3. Line note: `ermite mr comment <repo> <n> -m "text" --path <file> --line 10[-14] --side new`. Without `--path` it is general. The line must be in the diff. Markdown; `^N` refers to note N.
4. Verdict (`mr:review`): `--approve` or `--request-changes`, message optional. Verdicts cannot be edited; a new one replaces the old. They never block a merge.
5. Merge only when the human asks. Draft (`WIP:`/`[WIP]`)? `ermite mr ready <repo> <n>` first. Then `ermite mr merge <repo> <n> --head <head_id> [--mode fast-forward|squash|merge] [--delete-branch]`. Squash and merge commits are signed by the server key.

- Checks on the head must pass (none failed, queued or running); a head with no checks can merge. Never use `--ignore-checks` unless asked (`mr:force-merge`, not with `--auto`).
- `--auto` merges once CI passes (needs `repo edit --allow-auto-merge`); `--cancel-auto` stops it. A new push cancels it.
- Refused when the branch moved since `head_id` (review again), when fast-forward is impossible (rebase, or use squash/merge), on conflicts, or when the repo disallows the mode. `mr show` prints the mergeability line.

### Act on a review

Notes show as soon as they are written, so a review ends with its verdict. Act on the verdict, not on each note (`ermite watch --json`: `comment_added` with `verdict`). On `changes_requested`: read open threads in `mr show --json` (roots with `resolved: false`), fix, push, then reply `ermite mr comment <repo> <n> --reply <id> -m "Fixed in ^N"`. Do not resolve the reviewer's threads. Edit or delete only your own notes (`mr edit-comment`, `mr delete-comment`). An approval goes stale after a push; changes requested stay until a new verdict.

Open one: `ermite mr create <repo> --source <branch> --title "..." [--description-file f.md|-]` (prints the MR URL). After new pushes, `mr show --since-last-read` shows only changes since `--mark-read`.

## Issues

Enable: `ermite repo edit <repo> --issues owner|public`. Then `issue create <repo> --title T [--body-file f|-]`, `issue list --state open|closed|resolved|all`, `issue show <repo> <n> --json`, `issue comment`, `issue close` (not fixed), `issue resolve` (fixed; `issues.resolve` feature), `issue reopen`. Writes need `issue:write`. Merging an MR with `Fixes #N` resolves issue N.

Edit text with `issue edit --title/--body-file` or `mr edit --title/--description-file`. Body text comes only from files (`-` = stdin, 1 MiB, empty file clears).

## Tokens

```sh
ermite token create my-agent --preset agent --repo <repo> --expires 30d
```

The secret prints once. Presets: `agent` (`repo:write`, `mr:write`), `ci` (`repo:read`), `runner` (`runner:register`), `cli` (every scope). Add `--scope mr:review` for verdicts, `--scope mr:merge` to merge with passing checks. A token never grants more than its creator.

## CI

New branch pushes supersede older runs of the same workflow and branch; MR runs supersede runs of the same workflow and MR. A superseded run is cancelled, not failed. Watch the latest run after pushing. Explicit workflow concurrency overrides this default; repository owners can disable it with `repo edit --ci-auto-cancel=false`.

Workflows live in `.github/workflows` (GitHub Actions syntax).

```sh
ermite run list <repo> [--workflow ci.yml] [--branch B] [--status completed]
ermite run view <repo> <id> [--log [--job J]] [--summary]   # jobs, steps, annotations
ermite run watch <repo> <id> [--log]                        # exit 0 only on success
ermite run rerun <repo> <id> [--failed]                     # ci:write
ermite workflow run release.yml --repo <repo> -f version=1.2.3
ermite workflow disable <repo> codeql.yml                   # works before the first push
```

Secrets and variables: `ermite secret set NAME --repo R [--env E] < file` (values never readable), `ermite variable set|get NAME --repo R`. Scope order: environment > repository > `--instance`.

## Releases from CI

In a job, `GITHUB_API_URL` and `GITHUB_SERVER_URL` point at an address the job can reach. `softprops/action-gh-release` works as is. GoReleaser and tools with hard-coded GitHub URLs must read these variables (the public URL alone times out):

```yaml
github_urls:
  api: '{{ or (index .Env "GITHUB_API_URL") "https://git.example.org/-/github/api" }}/'
  upload: '{{ or (index .Env "GITHUB_API_URL") "https://git.example.org/-/github/api" }}/uploads/'
  download: '{{ or (index .Env "GITHUB_SERVER_URL") "https://git.example.org" }}/'
```

The workflow needs `permissions: contents: write` and `GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}`. Re-enable a disabled one with `ermite workflow enable <repo> release.yml`. Full example: `docs/releases.md` in the ermite repo.

## Catch up and follow

- `ermite journal --since 7d` lists MRs, issues, tags, releases and default-branch pushes (30 days kept).
- `ermite watch [repo] --json` prints one event per line, reconnects by itself; a `gap` event means reload the state.

## Run a server

```sh
token=$(head -c 32 /dev/urandom | base64)   # root credential: store it safely
docker run -d -p 127.0.0.1:7341:7341 -v ermite-data:/data \
  -e ERMITE_PUBLIC_URL=https://git.example.com -e ERMITE_ADMIN_TOKEN="$token" <ermite-image>
```

- Required: `ERMITE_DATA_DIR` (image: `/data`), `ERMITE_PUBLIC_URL`, `ERMITE_ADMIN_TOKEN` (32+ chars); git 2.45+.
- `ERMITE_SECRETS_KEY` enables secrets, mirrors and OIDC. TLS goes on a reverse proxy listed in `ERMITE_TRUSTED_PROXIES`.
- Health: `GET /-/health`. Owner browser: `ermite web login` (one-time link, ~60 s; 12 h session).
- Reads look wrong? Set `ERMITE_GIT_READER=cli` and restart.

## Output

`--json` gives stable fields. Errors: one line `ermite: ...` on stderr, exit 1. `ermite completion bash|zsh|fish` completes repo names.
