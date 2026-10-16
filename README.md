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
| `ERMITE_SECRETS_KEY` | empty | 32 random bytes in base64 (`head -c 32 /dev/urandom \| base64`) that encrypt CI secrets at rest (AES-256-GCM); keep it in the deployment secret store like `ERMITE_ADMIN_TOKEN`. Without it `ermite secret set` fails and jobs receive no secrets (variables and environments still work). The server refuses to start with another key than the one the stored secrets were sealed with; losing the key loses every secret (set them again), a backup without the key reveals none |
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

The URL GitHub-shaped clients use, `http://git.example.com/<owner>/<repo>` (`<owner>` is `ERMITE_OWNER`, with or without `.git`), is served too; any other owner is a 404. The token goes as the password of Basic with any user (`x-access-token`), or as `Authorization: Bearer <token>` or `token <token>`.

Use the admin token (`ERMITE_ADMIN_TOKEN`) or a stored token as the password; the username is ignored. Store it in a Git credential helper rather than in the URL. Pushing to a new name creates a private repository. Repository names are lowercase, `[a-z0-9._-]`. TLS is terminated by your proxy; list its network in `ERMITE_TRUSTED_PROXIES` so that failed-login limits see the real client address (`X-Forwarded-For`).

## Branch-limited tokens

For an AI agent that should propose changes, not publish them: `--branch-prefix agent/claude/` (repeatable, up to 8, each ending with `/`) limits a token to the branches under its prefixes.

- It can push, force-push and delete `refs/heads/agent/claude/…`. Any other ref (`main`, other branches, tags, notes, anything outside `refs/heads/`) refuses the whole push, and the message names the ref.
- It cannot create a repository (push to a new name, or `repo create`), delete one, or change its settings (default branch, visibility, description, merge modes, failing-checks rule): those decide what happens to branches outside its prefixes. It never has the rights of the owner (it cannot open web sessions even with every scope), and can only create tokens whose prefixes lie within its own.
- It opens, comments on and closes merge requests as usual; with `mr:merge` it can merge only into a branch under its prefixes, and delete only a source branch under them. You review and merge the agent's request.
- CI: it reads runs, jobs, logs and checks as any token that can read the repository. With `ci:write` it can cancel and re-run only the runs of branches under its prefixes, never those of `main`, other branches or tags. It never manages runners (`runner list|remove`, registration), even with `runner:register`: a runner runs every branch of every repository.
- `ermite token list` shows a `branches` column (`all` when unlimited); `--json` has `branch_prefixes`.
- This build uses storage schema 3 (2 added the branch prefixes of tokens; 3 the jobs of reusable workflows and the head commit of the runs of merge requests). Once it has started on your data, an older ermite refuses to start on it (it would read limited tokens as unrestricted, run the jobs of a called workflow as the caller's, and attach the checks of merge request runs to the wrong commit): to go back, restore the backup taken before the upgrade.

## Merge requests

A merge request proposes a branch for its target (the default branch unless set). The merge is refused when the branch moved since the reviewed head (`--head`) or when the target is not an ancestor of it. Fast-forward moves the target; squash writes one commit signed by the server's SSH key (`ermite signing-key show` prints the `allowed_signers` line for `git verify-commit`). `ermite repo edit --merge-modes` limits the modes per repository. `--delete-branch` (a checkbox on the web) deletes the source branch after the merge, unless it is the default branch or another open request uses it. Tokens need `mr:write` to open and comment, and `mr:merge` to merge (and delete the merged branch).

## CI

ermite runs the `.github/workflows/*.yml` workflows of a repository on `push` (a branch or a tag, and the merge of a merge request), on `pull_request` and `pull_request_target` (the merge requests, below), `workflow_dispatch`, `schedule`, `workflow_run`, `create` and `delete` (below) and shows each job as a check on the commit, the branch list and the merge request. CI is on once a runner is registered; there is no switch. Before that a push makes no run and no check.

Runs execute on [ermite-runner](https://git.bnema.dev/bnema/ermite-runner) hosts, which call ermite over its API. Create a token for the runner host with `ermite token create <label> --preset runner` (the `runner:register` scope, which also gives access to the code and the secrets of every repository: give it only to runners you operate), then register the runner with it. `ermite runner list` shows the runners and whether they are online; `ermite runner remove <id>` stops one. A job goes to a runner that has every label of its `runs-on`; a job no runner takes within 24 hours fails.

Every job gets a **repository token** (`github.token`, `ermitegh_…`) when a runner takes it: it reads the repository of the run, even a private one, over Git and the REST subset below, writes nothing, and ends with the job (completion, cancel, lost runner) or five minutes after the job's deadline. It is not a stored token: `ermite token list` never shows it and no scope manages it.

An action of `ERMITE_OWNER/<repo>` that exists on this server wins over the same name on github.com. It is resolved **on behalf of whoever pushed the run**: ermite records the pusher with the run (the admin token, the owner's session, or the stored token by its id) and, at every answer, asks whether that pusher can read the action's repository, with the same rule as everywhere else: a public repository, or `repo:read` on a repository of the token's list. The token is looked up again each time, so a token revoked or expired since reads nothing for the run. A repository the pusher cannot read answers like one that does not exist (`repository not found`), whatever the ref, so it does not reveal itself. A job can therefore use an action from another private repository only when its pusher could read it. The runner gets an **archive token** for each such action: it is bound to that repository and that commit and downloads that one archive (`/-/github/api/repos/<owner>/<repo>/tarball/<sha>`) and nothing else. Git over HTTP, the repository endpoint and the rest of the API see it as a caller who can read nothing, not even a public repository. It goes with its job, and with the repository it reads if that is deleted. At most eight archives are written at once for one client address (`429` beyond: a runner host runs several jobs, and all come from one address) and sixteen on the server (`503`); both answers carry `Retry-After: 5`. A client that stops reading is cut after two minutes without progress.

**`pull_request`.** A merge request is a pull request for the workflows. ermite keeps `refs/pull/<n>/head` at the head of its source branch and, while that head merges cleanly into the target, `refs/pull/<n>/merge` at a test merge commit (parents: the target tip, then the head; message `Merge <head> into <base>`). Both are served to `git fetch`, never listed as branches or tags, and refused on every push (for any token, a job's included). The activities are `opened` (the request is created), `synchronize` (its source branch is pushed; each push runs for the commit it pushed, with the rights of whoever pushed it) and `closed` (closed or merged, with `merged` true or false; it uses the last test merge commit, or the head when there is none, and is never computed again, even when the source branch is gone); `reopened` is accepted in `types:` but merge requests cannot be reopened. Without `types:` a workflow gets `opened`, `synchronize` and `reopened`. The run has `github.ref` `refs/pull/<n>/merge`, `github.sha` the merge commit (the workflow file is read from it, so `actions/checkout` gets the merged tree), `github.head_ref` and `github.base_ref` set, and a payload shaped like GitHub's `pull_request` webhook with the fields ermite has. `branches` and `paths` (and their `-ignore` forms) apply to the target branch and to the files changed between the merge base and the head. A head that conflicts with the target makes no run for that update. The checks of the run are on the head commit. The run is trusted (secrets, write token) only when whoever caused the event is the owner **and** the head and the base it runs were pushed by the owner: its workflow comes from the merge commit, so from both branches. Each is checked against the record of who set the branch to exactly that commit, taken when the event happens (see `schedule`); a branch with no record, or whose record names another commit, is untrusted. The owner opening, pushing on or closing the request of a branch an agent pushed (or one targeting an agent's branch) gets an untrusted run. A closing runs the last test merge commit, whose head and base are checked; when it is the merge of an older head than the closing's, it is untrusted. The closing a merge makes has the merger's trust when the merger set the target to the merge result and the head is the one merged, even after the source branch is deleted. A job token starts no run.

**`pull_request_target`.** The same activities, `types:` (default `opened`, `synchronize`, `reopened`), `branches` and `paths` filters and payload as `pull_request`, with `github.event_name` `pull_request_target`, but the workflow is read from the **default branch** and runs on it: `github.ref` is `refs/heads/<default>` and `github.sha` its tip as it was when the activity happened, whatever the head contains (a head that edits the workflow does not change what runs). It also runs when the head conflicts with the target. Its checks are on the head commit, like those of `pull_request`, so they show on the merge request and not on the default branch. The run is trusted (secrets, write token) only when **both** whoever caused the activity is the owner **and** the default branch tip was pushed by the owner (see `schedule`); the head's pusher does not matter, since none of its files run. This is stricter than GitHub, which gives the secrets to every pull request of the repository: here an agent that opens or pushes a merge request gets the run without secrets and with a read-only token. Re-runs and `workflow_run` keep or lower the trust as for any run. Its cache is read-only (the runner sets `ACTIONS_CACHE_MODE=read`, so `actions/cache` skips the save) and restores only the default branch's entries, never the merge ref's, which the `pull_request` runs of the request save from the head's code. Every other run restores from its own ref, then the target branch for a `pull_request`, then the default branch, and saves under its own ref. An entry keeps the trust of the run that saved it: a trusted run restores only entries trusted runs saved (the owner pushing on top of an agent's branch or merge request never restores what the agent's runs cached), an untrusted run restores both, and an untrusted run on the default branch (an agent's `workflow_run` or dispatch on it) saves nothing. Entries saved before ermite sent the cache scope (ermite-runner then used the run's ref whatever its trust) are not told apart: clear the runners' caches once after upgrading.

> **Warning** (GitHub's, and it applies here): running untrusted code on `pull_request_target` can leak the secrets and the write token, and poison caches. A trusted run that checks out the head (`actions/checkout` with `ref: ${{ github.event.pull_request.head.sha }}`, `git fetch` of `refs/pull/<n>/head`) and then builds, tests or installs it runs the head's code with them: a "pwn request". Use `pull_request` when the workflow builds the request's code; with `pull_request_target`, treat the checked-out head as data only and never execute it (build scripts, `npm install`, configuration files and tools it brings all count). See GitHub's [Securely using pull_request_target](https://docs.github.com/en/actions/reference/security/securely-using-pull_request_target) and [Preventing pwn requests](https://securitylab.github.com/research/github-actions-preventing-pwn-requests).

**`workflow_dispatch`.** `ermite workflow run <file> --repo <repo> [--ref <branch or tag>] [-f name=value …]` starts a run of a workflow that declares `workflow_dispatch` (the API is `RunService.DispatchWorkflow`, feature `ci.dispatch`; it needs `ci:write`). The file is its path or its name under `.github/workflows`, and it is read at the ref (the default branch when `--ref` is left out). The inputs are checked against the workflow: an undeclared input, a missing `required` input without a default, a `boolean` other than `true` or `false`, a `number` that is not a decimal number, a `choice` outside its `options`, and an `environment` that is not a valid environment name are refused; a `choice` without default takes its first option. A workflow has at most 25 inputs and their values at most 65,535 characters. In the run, `github.event.inputs` holds the values as strings and the `inputs` context as their declared types (`boolean`, `number`, else string). The actor is the caller. The run is trusted (secrets, write token) only when **both** the caller is the owner **and** the ref it runs on was moved to its current commit by the owner (see `schedule`): the owner dispatching an agent's branch gets an untrusted run, like the owner re-running an agent's run, and a ref with no record (pushed before ermite recorded pushers) is untrusted too. This holds for a tag as for a branch, and for a job token as for the owner. A token limited to branch prefixes can dispatch only on a branch under them and its run is never trusted. A job can dispatch with its repository token when its `permissions:` give `actions: write` (GitHub's exception to "events made with `GITHUB_TOKEN` start no run"), even without `contents: read`; a token that does not hold the permission is refused.

**`schedule`.** `on.schedule[].cron` (five fields) is read at the wall clock of the entry's `timezone`, an IANA time zone name such as `America/New_York` (UTC without one; the time zone database is built into ermite). A name that is not one makes the workflow invalid: its push records a failed run with the error. On a daylight saving spring-forward day, an entry whose time is skipped runs at the next valid time (a 02:30 entry runs at 03:00), as GitHub documents; on a fall-back day, a time that happens twice runs once, at its first occurrence (GitHub does not document it; this is cron's behaviour). The entries are read on the **default branch** only. Every minute the scheduler starts the workflows whose cron matches that minute, on the latest commit of the default branch; the payload is `{"schedule": "<cron>"}`. A minute is handled once: ticks missed while ermite was down or busy are not made up, and a workflow with several matching entries starts one run. The actor is **whoever last moved the default branch to its current commit** (a push or a merge), and the run is trusted only if that was the owner. ermite records who moves each branch and tag **before** it moves (in the pre-receive decision of a push, and in the merge under its lock; a record that cannot be written refuses the push), together with the commit it is for: the record counts only while the ref still points to that commit, so a late write cannot leave the owner's record on a tip an agent moved there (owner A, agent B, agent A). A tip with no record, a record for another commit (a push refused after its record was written) or an unreadable record runs untrusted, without secrets. If the owner and a token push the very same commit at the same time, the owner's record may remain: the content is the same and the owner pushed it. At most 64 repositories are looked at per second, in rotation, so a pass over more takes several seconds and never starves one. The schedules of a repository are read again when its default branch moves, and go with the repository.

**`workflow_run`.** A workflow that lists `workflow_run: workflows: [<name>]` runs when a run of a workflow with that `name` is `requested` (created) or `completed` (`types:` picks one; `in_progress` never happens). `branches` and `branches-ignore` apply to the head branch of the triggering run (the source branch of a merge request). The workflow is read from the **default branch** and runs on its latest commit; the payload has `workflow_run` (`id`, `name`, `head_branch`, `head_sha`, `event`, `status`, `conclusion`, `run_number`, `run_attempt`, `path`, `actor`) and `workflow`. The run inherits the actor and the trust of the triggering run and never gains any: a workflow_run of an untrusted run has no secrets. A chain stops after three levels of `workflow_run`.

**`create` and `delete`.** A push (or a merge that deletes its source branch) that creates or deletes a branch or a tag starts the workflows that declare `create` or `delete`, besides its `push` workflows. A `create` run is on the created ref: `github.ref` is it, `github.sha` its commit, and the workflow is read from that commit. A `delete` run is on the **default branch**, its latest commit, and its workflow is read from there: the deleted ref is gone. The payload has `ref` (the short name), `ref_type` (`branch` or `tag`), `pusher_type` (`user`), `repository` and `sender`; `create` adds `master_branch` (the default branch) and `description` (`null`). There are no filters, and `[skip ci]` does not apply. A push that creates (or deletes) more than three tags starts no `create` (or `delete`) run for them, as on GitHub. A `create` run is trusted only when the pusher is the owner **and** the record of the created ref names the commit it runs (see `schedule`); a `delete` run only when the deleter is the owner **and** the default branch tip was pushed by the owner, as for `pull_request_target` (the deleted ref's record went with it). A job token starts neither.

The event of a run is `push`, `pull_request`, `pull_request_target`, `workflow_dispatch`, `schedule`, `workflow_run`, `create` or `delete`: `ermite run list` and `view` and the Actions pages show it.

The Actions that run here call a small part of GitHub's REST API, served at `github.api_url` (`/-/github/api`): `GET /repos/{owner}/{repo}`, `/repos/{owner}/{repo}/hash-algorithm`, `/repos/{owner}/{repo}/tarball[/{ref}]` (streamed with 200, not redirected; a `HEAD` answers the headers without building the archive) and `GET /repos/{owner}/{repo}/pulls/{number}/files` (what `dorny/paths-filter` reads on a `pull_request`: up to 3000 files, `page` and `per_page` of at most 100, 30 by default, with the `Link` header; a job token needs `pull-requests: read`, else `403 Resource not accessible by integration`). The owner in these paths and in the Git URLs is matched without regard to case, as on GitHub. Any other path is GitHub's `404 {"message":"Not Found",…}`, so an action that needs more fails as on a GitHub instance without that feature.

Actions from github.com are resolved with `git ls-remote https://github.com/<owner>/<repo>` (no credentials, 20 s, 16 MiB of output, at most four at once, 60 s for a whole call). ermite hands git only the proxy settings of its own environment (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`, `ALL_PROXY` and their lower-case forms), so a server that reaches the Internet through a proxy sets them for `ermite serve`; nothing else of its environment, no credential, reaches git.

`ermite run list|view|watch|cancel|rerun` and the `Actions` tab of a repository read and control the runs; `cancel` and `rerun` need the `ci:write` scope. With `failing_checks_block` on (the default, `ermite repo edit --failing-checks-block=false`), a merge is refused while a check of the head failed or has not finished. `concurrency:` is supported at the workflow level (evaluated when the run starts) and at the job level (evaluated when the job is planned, once per matrix leg): a group is per repository and compared without regard to case, `queue: single` (the default) lets one run or job go and keeps one waiting, a newer waiting entry cancels the older one (`Canceling since a higher priority waiting request for '<group>' exists`), and `cancel-in-progress` (a boolean or an expression) also cancels the running one. With `queue: max`, up to 100 entries wait and run first in, first out; one more is cancelled (`Canceling since the concurrency group '<group>' already has 100 waiting requests`, ermite's wording). In a group that mixes both (GitHub does not say), ermite applies the rule of the entry that starts waiting: a `single` entry cancels every waiting entry, `max` ones included, and a `max` entry joins the end of the line. A job held back by a group shows `pending` in the API, the web pages and the CLI. A group that evaluates to nothing fails the job. `queue: max` with `cancel-in-progress: true` written as such makes the workflow invalid (its push records a failed run with the error, as GitHub's validation does); when either is an expression that evaluates to that pair, the job fails with the same message. The waiting line is stored with its head also in the field older ermite versions read, so a downgrade keeps the first waiting entry and cancels the others. Reusable workflows (`jobs.<id>.uses`) run: `./.github/workflows/x.yml` (the caller's repository, at the commit of the run), `owner/repo/.github/workflows/x.yml@ref` of a repository of this server when `owner` is `ERMITE_OWNER` and the actor of the run can read it (else the call fails as for a repository that does not exist), and otherwise a public repository on github.com, whose ref is resolved like the ref of an action and whose file is downloaded at that commit (at most 1 MiB). ermite expands the call when the job is planned: the called workflow must declare `on: workflow_call`, `with:` is checked against its inputs (types, `required`, defaults), `secrets:` passes the listed secrets, renamed as written, or all of them with `secrets: inherit` (a secret that is not passed is not there), and its `outputs` are available to the later jobs of the caller as `needs.<job>.outputs`. The jobs of the called workflow are named `<calling job> / <job>` and keyed `<calling job id>/<job id>`, wait for the `needs` of the calling job, and are repeated for each leg of its `strategy`; `permissions` can only be lowered by the called workflow. A call may nest four levels deep. `concurrency:` on the calling job and the workflow-level `concurrency:` of the called workflow (evaluated with the caller's `github` and the call's `inputs`) apply to the whole call, which holds the group until it ends, with `cancel-in-progress` as for any job. A workflow that uses another feature ermite does not support yet fails with `not supported yet: <feature>`, never silently. Logs are files under `$ERMITE_DATA_DIR/ci/logs`, and the job summaries (the Markdown a step writes to `GITHUB_STEP_SUMMARY`) under `$ERMITE_DATA_DIR/ci/summaries`; both go with their run (retention, repository deletion). Annotations (`::notice`, `::warning`, `::error`) are kept in the metadata store. The run page shows them in an Annotations box and the job page at their step; the summaries are rendered under the run with the README renderer (raw HTML dropped, `javascript:` links dropped). `ermite run view` prints an ANNOTATIONS section, and `ermite run view --summary` prints the summaries as Markdown.

**Artifacts** are not supported. `actions/upload-artifact` and `actions/download-artifact` v4 and later (`@actions/artifact` v2) refuse any server that is not github.com, `*.ghe.com` or `*.localhost` and fail with `GHESNotSupportedError: @actions/artifact v2.0.0+, upload-artifact@v4+ and download-artifact@v4+ are not currently supported on GHES.`, as on a GitHub Enterprise Server. The cache (`actions/cache`, `setup-go`/`setup-node` `cache:`) is served by the runner and works.

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
