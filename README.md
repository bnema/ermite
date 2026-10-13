# ermite

A single-user Git forge in one Go binary. ermite hosts bare repositories, serves clone and push over HTTPS, reviews branches as merge requests, and shows everything in a web interface with live updates.

Status: early development. `ermite serve` serves Git over HTTP (clone, fetch, push), the API, `GET /-/health`, and the web: repositories, an expandable file tree with a repository-wide fuzzy file finder, files, commits, refs and merge requests. Visitors read public repositories; the owner signs in with a one-time link from the CLI to see private ones and to review and merge.

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
| `ERMITE_OWNER` | `owner` | owner login that CI shows in `github.repository` (`<owner>/<repo>`) and `github.repository_owner`; follows the rules of repository names |
| `ERMITE_CI_LOG_RETENTION` | `90d` | how long CI runs, jobs, checks and logs are kept: `Nd` (days) or a Go duration such as `720h`, at least one hour |
| `ERMITE_CI_POLL_TIMEOUT` | `50s` | how long the server holds a runner's `FetchJob` call when no job is queued, from `1s` to `55s`; set it below the response timeout of a reverse proxy in front of ermite (Gordon cuts a response that has no headers after 30 s, so use `25s` behind it) |

`ermite version` prints the version (and the server's when a server URL is configured, see Remote CLI).

## Remote CLI

The same binary drives a running server over its API. It reads `ERMITE_URL` (the server address, `http://` or `https://`) and `ERMITE_TOKEN` (a bearer token; optional for public reads). A token sent over plain `http://` to a host that is not loopback triggers a warning on every call.

Each setting comes from the environment or from a config file, the environment first:

| Setting | Environment | Config file key |
|---|---|---|
| server address | `ERMITE_URL` | `url` |
| bearer token (optional for public reads) | `ERMITE_TOKEN` | `token.cmd`, a command that prints it |

The file is `~/.config/ermite/config` (`$XDG_CONFIG_HOME/ermite/config` when set; `~/Library/Application Support/ermite/config` on macOS and `%AppData%\ermite\config` on Windows; `ERMITE_CONFIG` names another file). `ermite config show` prints the path in use. It holds no secret: `token.cmd` is a command line that fetches the token from your secret manager (pass, gopass, op, bw, secret-tool, …).

```ini
# ~/.config/ermite/config
url       = https://git.example.com
token.cmd = pass show ermite/token
```

```sh
ermite config set url https://git.example.com
ermite config set token.cmd 'pass show ermite/token'   # or: gopass show -o ermite/token, op read op://vault/ermite/token, secret-tool lookup service ermite
ermite config show [--json]                             # path and stored values; unset removes a key
```

`token.cmd` runs with `sh -c` (`cmd /C` on Windows) only when a command needs the token, at most once per run, with your environment and, when standard input is a terminal, that terminal (so a pinentry or biometric prompt works; otherwise it reads nothing). On a timeout or when ermite is interrupted or terminated, the command and everything it started are killed (when standard input is a terminal, only the shell is, so that prompts keep working). It must print the token alone on one line, in under 4 KiB, within 60 seconds. It is not run when `ERMITE_TOKEN` is set, nor by `ermite version` or `ermite config`, and not when the config file, or the directory holding it, can be written by other users or the file belongs to another user (`ermite config show` and `set` warn about it). On Windows this check does not exist: keep the file in your own profile, the default place. It belongs to the server of the file: when `ERMITE_URL` names a different server than the file's `url`, it is not run and the call is anonymous (with a notice; set `ERMITE_TOKEN` to authenticate there). A failure is reported in one line without its output. Shell completion never runs it: it is anonymous unless `ERMITE_TOKEN` is set.

```sh
export ERMITE_URL=https://git.example.com ERMITE_TOKEN=ermite_...
ermite repo list | show <name> | create <name> [--description …] [--visibility public] | edit <name> [--description …] [--visibility …] [--default-branch …] | delete <name> [--yes]
ermite token create <label> [--preset cli|agent|ci|runner] [--scope repo:read …] [--repo name …] [--branch-prefix agent/x/ …] [--expires 30d]   # prints the secret once
ermite token list | revoke <id>
# a token for an AI agent that cannot touch main:
ermite token create claude --preset agent --scope repo:read --repo myrepo --branch-prefix agent/claude/
ermite mr create <repo> --source <branch> --title … | list <repo> | show <repo> <n> [--diff]
ermite mr comment <repo> <n> -m … [--path … --line … --side old|new] | resolve <repo> <n> <comment>
ermite mr merge <repo> <n> --head <id> [--squash] [--delete-branch] | close <repo> <n>
ermite run list <repo> [--workflow …] [--branch …] [--status …] | view <repo> <id> [--attempt n] [--log [--job …]]
ermite run watch <repo> <id> [--log] | cancel <repo> <id> | rerun <repo> <id> [--failed]
ermite runner list | remove <id>
ermite web login                # prints a one-time sign-in link (60 s)
ermite web logout --all         # signs every browser out
ermite web passkey list | remove <id>   # the owner's passkeys; removing one signs every browser out
ermite signing-key show         # public key and allowed_signers line of squash commits
ermite config show [--json] | set <url|token.cmd> <value> | unset <key>   # the CLI config file
ermite version                  # client and server, warns about missing server features
ermite watch [repo] [--json]    # follows events, reconnects and resumes
```

An agent skill describing the CLI and the forge lives in [`skills/ermite`](skills/ermite/SKILL.md); link or copy that folder into your agent's skills directory.

`--json` on `repo list|show|create|edit`, `token create|list`, `run list|view|cancel|rerun`, `runner list` and `version` prints stable field names, listed in each command's `--help`; `watch --json` prints one object per line. Errors are one line on standard error (`ermite: …`) and exit status 1. `ermite completion bash|zsh|fish` prints a shell completion script that completes repository names from the server.

## Git over HTTP

```sh
git clone http://git.example.com/<repo>.git          # public repositories need no credentials
git push http://u:<token>@git.example.com/<repo>.git main
```

Use the admin token (`ERMITE_ADMIN_TOKEN`) or a stored token as the password; the username is ignored. Store it in a Git credential helper rather than in the URL. Pushing to a new name creates a private repository. Repository names are lowercase, `[a-z0-9._-]`. TLS is terminated by your proxy; list its network in `ERMITE_TRUSTED_PROXIES` so that failed-login limits see the real client address (`X-Forwarded-For`).

## Branch-limited tokens

For an AI agent that should propose changes, not publish them: `--branch-prefix agent/claude/` (repeatable, up to 8, each ending with `/`) limits a token to the branches under its prefixes.

- It can push, force-push and delete `refs/heads/agent/claude/…`. Any other ref (`main`, other branches, tags, notes, anything outside `refs/heads/`) refuses the whole push, and the message names the ref.
- It cannot create a repository (push to a new name, or `repo create`), delete one, or change its settings (default branch, visibility, description, merge modes, failing-checks rule): those decide what happens to branches outside its prefixes. It never has the rights of the owner (it cannot open web sessions even with every scope), and can only create tokens whose prefixes lie within its own.
- It opens, comments on and closes merge requests as usual; with `mr:merge` it can merge only into a branch under its prefixes, and delete only a source branch under them. You review and merge the agent's request.
- CI: it reads runs, jobs, logs and checks as any token that can read the repository. With `ci:write` it can cancel and re-run only the runs of branches under its prefixes, never those of `main`, other branches or tags. It never manages runners (`runner list|remove`, registration), even with `runner:register`: a runner runs every branch of every repository.
- `ermite token list` shows a `branches` column (`all` when unlimited); `--json` has `branch_prefixes`.
- This build stores tokens in storage schema 2. Once it has started on your data, an older ermite refuses to start on it (it would read limited tokens as unrestricted): to go back, restore the backup taken before the upgrade.

## Merge requests

A merge request proposes a branch for its target (the default branch unless set). The merge is refused when the branch moved since the reviewed head (`--head`) or when the target is not an ancestor of it. Fast-forward moves the target; squash writes one commit signed by the server's SSH key (`ermite signing-key show` prints the `allowed_signers` line for `git verify-commit`). `ermite repo edit --merge-modes` limits the modes per repository. `--delete-branch` (a checkbox on the web) deletes the source branch after the merge, unless it is the default branch or another open request uses it. Tokens need `mr:write` to open and comment, and `mr:merge` to merge (and delete the merged branch).

## CI

ermite runs the `.github/workflows/*.yml` workflows of a repository on `push` (a branch or a tag, and the merge of a merge request) and shows each job as a check on the commit, the branch list and the merge request. CI is on once a runner is registered; there is no switch. Before that a push makes no run and no check.

Runs execute on [ermite-runner](https://git.bnema.dev/bnema/ermite-runner) hosts, which call ermite over its API. Create a token for the runner host with `ermite token create <label> --preset runner` (the `runner:register` scope, which also gives access to the code and the secrets of every repository: give it only to runners you operate), then register the runner with it. `ermite runner list` shows the runners and whether they are online; `ermite runner remove <id>` stops one. A job goes to a runner that has every label of its `runs-on`; a job no runner takes within 24 hours fails.

`ermite run list|view|watch|cancel|rerun` and the `Actions` tab of a repository read and control the runs; `cancel` and `rerun` need the `ci:write` scope. With `failing_checks_block` on (the default, `ermite repo edit --failing-checks-block=false`), a merge is refused while a check of the head failed or has not finished. A workflow that uses a feature ermite does not support yet (reusable workflows, `environment:`, `concurrency`) fails with `not supported yet: <feature>`, never silently. Logs are files under `$ERMITE_DATA_DIR/ci/logs`.

## Web sign-in

Two ways in, both for the owner, both giving the same session cookie for 12 hours. Neither works over plain `http` except on localhost: the cookie is `Secure`.

- **Passkey** (a phone, a laptop, a security key): open `/-/login` and press "Sign in with a passkey". Nothing to type, no terminal. A passkey is added once from `/-/passkeys` while signed in.
- **Link**: `ermite web login`, run with the admin token or a token with every scope, prints a link that works once within 60 seconds. Opening it and confirming signs the browser in (never longer than the token that asked for the link). It is also how to sign in the first time, before any passkey exists.

Passkeys need `ERMITE_PUBLIC_URL` to be `https` (or `http://localhost`) with a **host name, not an IP address**: WebAuthn ties a passkey to that name, and a passkey registered for one name does not work on another. On `http://127.0.0.1:…` the server starts without passkeys (the log says `passkeys disabled`, the routes and links are absent, `ermite version` does not list `passkeys`). Up to 20 passkeys can be registered; each asks for user verification (a fingerprint, a face or a PIN), and the server stores only the public key. A security key that reports a signature counter is checked for cloning; synced passkeys (iCloud, Google, a password manager) report none, so they are not.

`ermite web passkey list` shows them, `ermite web passkey remove <id>` removes one. Removing a passkey, `ermite web logout --all`, or revoking a token with every scope ends every session, on every device. Browser writes require the session and an `Origin` equal to `ERMITE_PUBLIC_URL`. If you lose every device, `ermite web login` still works from a terminal.

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
- The web adapter, like the API and Git over HTTP, calls the use cases through `internal/ports` with the principal of the request (anonymous, or the owner with a valid session); it never imports the generated API code.
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
