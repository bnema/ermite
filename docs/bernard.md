# Bernard

Bernard watches the dependencies of your repositories and lists the updates that are worth looking at in the owner's Inbox.

## What it does

- Once a day (`ERMITE_DEPS_CHECK_AT`, 06:00 UTC by default) it checks every repository that is not [archived](cli.md#archived-repositories). Archived repositories are skipped.
- It reads Go modules (`go.mod`) and GitHub Actions (`uses:` in workflows and `action.yml`). Vendored modules, `replace`d modules and actions pinned to a commit are left alone.
- For each dependency it proposes the highest version that is older than the cooldown (`ERMITE_DEPS_COOLDOWN`, 72 hours by default) and has no malicious-package (`MAL-`) advisory. A version that cannot be checked is not proposed. Go modules never move to a new major path, and a Go module version is proposed only if its `go.mod` does not need a newer `go` directive than the requiring module's.
- A check that finds a version the previous one did not publishes a `deps.updates_found` event, delivered to the owner only.

`ermite deps check <repo>` runs the same check now. See [Configuration](configuration.md) for the `ERMITE_DEPS_*` settings; `ERMITE_DEPS_ENABLED=0` turns Bernard off.

## How updates are applied

GitHub Actions updates are applied by the server. Go module updates are applied by a runner: Bernard queues a job for the repository at the tip of the default branch and waits for a runner that offers the `deps` job kind and has the label set by `ERMITE_DEPS_RUNNER_LABEL` (`ubuntu-latest` by default). The job gets a read-only repository token for that commit, no secrets, no variables and no ID token. The runner runs `go get` and `go mod tidy` for the requested versions and reports the changed `go.mod` and `go.sum` files.

Bernard accepts a report only if it changes `go.mod` and `go.sum` of the requested directories, keeps `replace`, `exclude`, `retract`, `go` and `toolchain` as they were, sets each requested module to exactly the requested version and leaves the other direct requirements alone. It then commits the files, together with the Actions updates, on `bernard/deps` on top of the commit the job ran on, and opens or updates the merge request.

A job has `ERMITE_DEPS_JOB_TIMEOUT` (10 minutes by default) to finish. Nothing is retried: a failed job leaves the card as it was, and you can click **Open MR** again. A job that no runner takes within the same time fails too. Older runners, which do not offer the `deps` kind, never receive these jobs.

## The Inbox card

The Inbox shows one card per repository with pending updates, after the running workflows. Each line reads `name current → latest (kind)`, where the kind is patch, minor or major, with a **new** badge for a version that appeared since the last check and the directory when it is not the repository root. The card shows `APPLYING` while a runner prepares Go module updates, and `FAILED` with the reason when it failed: the error of the runner, a rejected report, a timeout or a server restart, followed by the end of the job log. The counters at the top include the number of repositories with updates. The card updates live.

Three buttons act on the card:

- **Open MR** opens the Bernard merge request, or updates it.
- **Ignore** hides exactly the listed versions. A newer version shows up again.
- **Snooze** hides the card for seven days (`ERMITE_DEPS_SNOOZE`), then it comes back.

Each button sends the versions you saw. If a check changed the list in the meantime, or an update is being applied, the server refuses and you reload. Only the owner sees the Inbox and these buttons; everyone else gets a 404.

The same actions are available as `ermite deps list|check|open|ignore|snooze` ([CLI](cli.md#dependency-updates)) and as `DepsService` in the API.
