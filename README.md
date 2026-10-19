<h1 align="center">ermite</h1>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-AGPL--3.0-blue?style=flat-square" alt="License: AGPL-3.0"></a>
  <a href="https://github.com/bnema/ermite/commits/main"><img src="https://badgen.net/github/last-commit/bnema/ermite/main?icon=github" alt="Last commit"></a>
  <a href="https://github.com/bnema/ermite/stargazers"><img src="https://badgen.net/github/stars/bnema/ermite?icon=github" alt="GitHub stars"></a>
</p>

<p align="center">A single-user Git forge in one Go binary.</p>

> Read-only mirror. Development happens at git.bnema.dev; issues and pull requests are closed here.

---

## Features

- **GitHub Actions, as is**: your `.github/workflows` run unchanged, marketplace actions included
- **Isolated runner**: [ermite-runner](https://github.com/bnema/ermite-runner) runs each job in a Firecracker microVM when KVM is available, else in a container
- **CLI-first**: repositories, merge requests, reviews and CI all work from the `ermite` CLI, for you and for AI agents
- **Safe for agents**: an agent token can only push its own branches; you review and merge
- Git over HTTPS, web interface with live updates

Status: early development.

## GitHub Actions

Push a repository with a `.github/workflows` folder and its workflows run, the same YAML you use on GitHub:

- Events: `push`, `pull_request`, `pull_request_target`, `workflow_dispatch`, `schedule`, `workflow_run`, `create`, `delete`
- Matrix, `needs`, `concurrency`, reusable workflows, job outputs and summaries
- Secrets, variables, environments and OIDC (`id-token: write`)
- `actions/checkout`, `actions/cache`, `setup-*`, `upload-artifact` / `download-artifact` v4
- Actions from github.com or from your own ermite repositories
- Each job is a check on the commit and the merge request; failing checks block the merge

A feature ermite does not support fails with `not supported yet: <feature>`, never silently.

### Runner

Jobs run on [ermite-runner](https://github.com/bnema/ermite-runner) hosts, one fresh sandbox per job:

- **microVM** (Firecracker) when `/dev/kvm` is available: each job gets its own kernel, destroyed at the end
- **container** with its own Docker daemon otherwise

```sh
ermite token create runner-1 --preset runner   # then register the runner with this token
ermite runner list
```

CI turns on as soon as a runner is registered. Details: [docs/ci.md](docs/ci.md).

## Run

```sh
docker build -t ermite .
token=$(head -c 32 /dev/urandom | base64)   # keep it: it is the root credential
docker run -d -p 127.0.0.1:7341:7341 -v ermite-data:/data \
  -e ERMITE_PUBLIC_URL=http://localhost:7341 -e ERMITE_ADMIN_TOKEN="$token" ermite
```

Without Docker, set `ERMITE_DATA_DIR`, `ERMITE_PUBLIC_URL` and `ERMITE_ADMIN_TOKEN`, then run `ermite serve`. Requires git 2.45 or newer. Put a TLS proxy in front of it.

All settings: [docs/configuration.md](docs/configuration.md).

## Use

Point the CLI at the server:

```sh
ermite config set url https://git.example.com
ermite config set token.cmd 'pass show ermite/token'   # any command that prints the token
```

Then:

```sh
ermite repo create myrepo
git push https://x:<token>@git.example.com/myrepo.git main   # prefer a credential helper
ermite mr create myrepo --source feature --title "Add feature"
ermite web login   # one-time link to sign in to the web interface
```

Pushing to a new name also creates a private repository.

`ermite mr create` and `ermite mr close` print a confirmation followed by the full
merge request web URL on its own line. With `--json`, merge request objects
(create, show, list, close and merge) include `url` alongside the other fields.

## Agent workflow

You and the agent use the same CLI. The difference is the token: the agent's is limited to its own branches. It can never push `main`, tags or settings, and its CI runs get no secrets.

**1. You give the agent a token** limited to `agent/claude/*`:

```sh
ermite token create claude --preset agent --scope repo:read --repo myrepo --branch-prefix agent/claude/ --expires 30d
```

**2. The agent pushes and opens a merge request:**

```sh
export ERMITE_URL=https://git.example.com ERMITE_TOKEN=<agent token>
git push origin agent/claude/fix-login
ermite mr create myrepo --source agent/claude/fix-login --title "Fix login redirect"
ermite run watch myrepo <run id>         # follow its CI
```

**3. You review:**

```sh
ermite mr show myrepo 1 --diff
ermite mr comment myrepo 1 -m "Handle the empty case" --path auth.go --line 42 --side new
```

**4. The agent reads your comments and pushes again:**

```sh
ermite watch myrepo --json               # live events: pushes, merge requests, comments
ermite mr show myrepo 1 --since-last-read --mark-read
```

**5. You merge** (or click Merge in the web interface):

```sh
ermite mr merge myrepo 1 --head <head id> --mode squash --delete-branch
```

Give the agent the [`skills/ermite`](skills/ermite/SKILL.md) skill so it knows the CLI. Rules of agent tokens: [docs/tokens.md](docs/tokens.md).

## Documentation

- [Configuration](docs/configuration.md): environment variables
- [CLI](docs/cli.md): config file, commands, JSON output
- [Git over HTTP](docs/git-http.md): URLs and authentication
- [Tokens](docs/tokens.md): scopes, presets, branch-limited tokens
- [Merge requests](docs/merge-requests.md)
- [Issues](docs/issues.md)
- [Commit signatures](docs/commit-signatures.md): SSH and OpenPGP badges, trusted keys
- [Web sign-in](docs/web-sign-in.md): passkeys and one-time links
- [CI](docs/ci.md): runners, trust, concurrency, reusable workflows
  - [CI events](docs/ci-events.md)
  - [OIDC](docs/ci-oidc.md)
  - [Artifacts](docs/ci-artifacts.md)
- [Development](docs/development.md): build, tests, architecture

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE).
