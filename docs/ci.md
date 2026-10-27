# CI

ermite runs the `.github/workflows/*.yml` of a repository, like GitHub Actions. Each job shows as a check on the commit, the branch list and the merge request.

See also: [events](ci-events.md), [OIDC](ci-oidc.md), [artifacts](ci-artifacts.md).

## Runners

Jobs run on [ermite-runner](https://github.com/bnema/ermite-runner) hosts: a Firecracker microVM per job when KVM is available, else a container.

```sh
ermite token create runner-1 --preset runner   # register the runner with this token
ermite runner list                             # online or not
ermite runner remove <id>
```

- CI is on once a runner is registered. Before that, pushes make no run.
- The `runner` preset can read the code and secrets of **every** repository. Use it only on runners you operate.
- A job goes to a runner that has every label of its `runs-on`. A job no runner takes within 24 hours fails. A job whose `runs-on` matches no registered runner (online or offline) fails at once with `no registered runner matches runs-on: [...]`; one matched only by an offline runner waits up to that limit.

## Control runs

```sh
ermite run list <repo>
ermite run view <repo> <id> [--log] [--summary]
ermite run watch <repo> <id>
ermite run cancel <repo> <id>          # needs ci:write
ermite run rerun <repo> <id> [--failed]
```

### Disable a workflow

```sh
ermite workflow list <repo> [--json]
ermite workflow disable <repo> <file>   # needs repo:admin
ermite workflow enable <repo> <file>
```

Like `gh workflow disable`: a disabled workflow starts no run for any event (`push`, `pull_request`, `pull_request_target`, `schedule`, `workflow_run`, `create`, `delete`, `workflow_dispatch`). Runs already started go on, and events missed while it was disabled are not replayed.

- `<file>` is `release.yml` or `.github/workflows/release.yml`. The state belongs to the repository and the path, not to a branch, and the file need not exist: disable a release workflow right after `ermite repo create`, before the first push, so pushing tags starts nothing. The files stay identical to their GitHub copies.
- `list` shows the files of the default branch with their name and state, and the disabled files that branch lacks. Needs `repo:read` for a private repository.
- The Actions tab names the disabled workflows, and the page of a run says when its workflow is disabled.
- A disabled **reusable** workflow (`on: workflow_call`) can still be called with `uses:` from an enabled workflow: it then runs inside the caller's run, not as a run of its own, as on GitHub. Disable the caller to stop it.
- The state is checked again in the transaction that stores a run, so a disable takes effect for every run not yet stored, even one being planned at that moment.

The **Actions** tab of a repository shows the same. A merge needs passing checks on the head: none failed, queued or running. A head with no checks can merge. `ermite mr merge <repo> <n> --head <id> --ignore-checks` bypasses checks, needs `mr:force-merge`, and cannot be combined with `--auto`.

## Trust

A run is **trusted** (secrets, write token, OIDC) only when the owner caused it **and** the owner pushed the code it runs.

- ermite records who moved each branch and tag to its current commit. A ref with no record, or a record for another commit, is untrusted.
- An agent's push or merge request gets an untrusted run: no secrets, read-only token.
- The owner re-running or dispatching an agent's branch still gets an untrusted run.
- A job token starts no run (except `workflow_dispatch` with `actions: write`).

Per-event rules are in [ci-events.md](ci-events.md).

## Job token

Every job gets `github.token` (`ermitegh_…`). It expires with the job and is not listed in `ermite token list`.

- By default it reads the run's repository (even private) over Git and the REST subset below, and writes nothing.
- In a trusted run, `permissions: contents: write` lets it push branches and tags to that repository and publish [releases](releases.md). A push that changes `.github/workflows` is refused, as on GitHub.
- `pull-requests: write` gives it `mr:write` on that repository.
- An untrusted run gets every `write` lowered to `read`.
- A push or a tag made with it starts no run. A workflow that creates a tag and releases it does both in the same run (`needs:`).

## Actions

- `ERMITE_OWNER/<repo>` on this server wins over the same name on github.com. It resolves only if whoever pushed the run can read that repository; otherwise it answers `repository not found`.
- Other actions come from github.com (`git ls-remote`, no credentials). If the server needs a proxy, set `HTTPS_PROXY` / `NO_PROXY` for `ermite serve`.
- The cache (`actions/cache`, `setup-*` `cache:`) works. Trusted runs only restore entries saved by trusted runs.

### REST API subset

Served at `github.api_url` (`/-/github/api`):

- `GET /repos/{owner}/{repo}`
- `GET /repos/{owner}/{repo}/hash-algorithm`
- `GET /repos/{owner}/{repo}/tarball[/{ref}]`
- `GET /repos/{owner}/{repo}/pulls/{number}/files` (for `dorny/paths-filter`; needs `pull-requests: read`)

Any other path is GitHub's `404`, so an action that needs more fails as on a GitHub instance without that feature.

## Concurrency

`concurrency:` works at workflow and job level, per repository, case-insensitive.

- `queue: single` (default): one runs, one waits; a newer waiting entry cancels the older.
- `queue: max`: up to 100 wait, first in first out.
- `cancel-in-progress` also cancels the running one. It cannot be combined with `queue: max`.
- A held job shows as `pending`.

## Reusable workflows

`jobs.<id>.uses` accepts:

- `./.github/workflows/x.yml` (same repository, same commit);
- `owner/repo/.github/workflows/x.yml@ref` on this server, if the run's actor can read it;
- a public repository on github.com.

`with:`, `secrets:` (or `secrets: inherit`) and `outputs` work as on GitHub. Called jobs are named `<caller> / <job>`. Up to four levels deep. `permissions` can only be lowered.

## Logs, summaries, annotations

- Logs: `$ERMITE_DATA_DIR/ci/logs`. Summaries (`GITHUB_STEP_SUMMARY`): `$ERMITE_DATA_DIR/ci/summaries`. Both are removed with their run.
- Annotations (`::notice`, `::warning`, `::error`) show on the run and job pages and in `ermite run view`.
- Retention: `ERMITE_CI_LOG_RETENTION` (default 90 days).

## Unsupported features

A workflow that uses a feature ermite does not support fails with `not supported yet: <feature>`, never silently.
