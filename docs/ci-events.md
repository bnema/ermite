# CI events

| Event | Workflow read from | Runs on | Trusted when the owner caused it and… |
|---|---|---|---|
| `push` | pushed commit | pushed ref | pushed it |
| `pull_request` | test merge commit | `refs/pull/<n>/merge` | pushed both head and base |
| `pull_request_target` | default branch | default branch tip | pushed the default branch tip |
| `workflow_dispatch` | chosen ref | chosen ref | moved that ref to its commit |
| `schedule` | default branch | default branch tip | last moved the default branch |
| `workflow_run` | default branch | default branch tip | inherits the triggering run, never more |
| `create` | created ref | created ref | pushed it |
| `delete` | default branch | default branch tip | pushed the default branch tip |
| `dependencies` | none (Bernard starts one job) | default branch tip | never trusted |

`ermite run list`, `run view` and the Actions pages show the event of each run.

## `dependencies`

[Bernard](bernard.md) starts it to apply dependency updates. The run has one job, **Update dependencies**, with no workflow file: it is ready at once and goes to a runner that offers the `deps` job kind. It is never trusted, so its token is read-only and it gets no secrets. It posts no check, starts no `workflow_run` workflow and cannot be re-run. Workflow `Dependencies`, actor `bernard`.

## `pull_request`

A merge request is a pull request for workflows.

- `refs/pull/<n>/head` follows the source branch. `refs/pull/<n>/merge` is a test merge with the target, while it merges cleanly. Both can be fetched, never pushed.
- Activities: `opened`, `synchronize` (a push on the source branch), `closed` (`merged` true or false). Default: `opened`, `synchronize`, `reopened`.
- `branches` filters the target; `paths` filters files changed between the merge base and the head.
- A head that conflicts with the target makes no run.
- Checks are on the head commit.

## `pull_request_target`

Same activities, filters and payload as `pull_request`, but the workflow comes from the **default branch** and runs on it. A head that edits the workflow changes nothing. Runs even on conflict.

Stricter than GitHub: a merge request opened or pushed by an agent gets no secrets.

Its cache is read-only and restores only default-branch entries.

> **Warning:** never build, test or install the head's code in a trusted `pull_request_target` run (checking out `github.event.pull_request.head.sha` and running it). That leaks secrets: a "pwn request". Use `pull_request` to build a request's code. See GitHub's [Securely using pull_request_target](https://docs.github.com/en/actions/reference/security/securely-using-pull_request_target).

## `workflow_dispatch`

```sh
ermite workflow run <file> --repo <repo> [--ref <branch|tag>] [-f name=value …]
```

- Needs `ci:write`. `<file>` is a path or a name under `.github/workflows`. `--ref` defaults to the default branch. A disabled workflow (`ermite workflow disable`) is refused with `failed_precondition`.
- Inputs are checked against the workflow (`required`, `boolean`, `number`, `choice`, `environment`). Up to 25 inputs.
- A branch-limited token can dispatch only on its branches, and never gets a trusted run.
- A job can dispatch with its own token when its `permissions:` give `actions: write`.

## `schedule`

- `cron` (five fields) with an optional IANA `timezone` (UTC by default). An invalid name fails the push's run.
- Read on the default branch only; runs on its latest commit.
- Missed minutes (server down) are not made up. Several matching entries start one run.
- DST: a skipped time runs at the next valid time; a repeated time runs once.

## `workflow_run`

- `workflows: [<name>]`, `types: requested | completed`. `branches` filters the head branch of the triggering run.
- Payload: `workflow_run` and `workflow`.
- Chains stop after three levels.

## `create` and `delete`

- Start on a push that creates or deletes a branch or tag (also a merge with `--delete-branch`).
- No filters; `[skip ci]` does not apply. A push of more than three tags starts none.
- Payload: `ref`, `ref_type`, `pusher_type`, `repository`, `sender`; `create` adds `master_branch` and `description`.

## Mirror events

Not workflow triggers: `ermite watch` reports them when a [push mirror](mirror.md) changes health, once per incident.

- `mirror.failed`: the mirror started failing or was paused. Payload: `repo`, `state` (`failing` or `paused`), `failure_class` (`diverged`, `auth_denied`, `remote_not_found`, `host_key`, `transient`, `internal`). In `ermite watch --json` they are `mirror_failed` (with `state`, `failure_class`) and `mirror_recovered`.
- `mirror.recovered`: a failing or paused mirror pushed successfully. Payload: `repo`.

The payload carries neither the remote URL, the reason nor the error detail; `ermite mirror status` shows the reason to repository admins.
