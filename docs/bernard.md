# Bernard

Bernard watches the dependencies of your repositories and lists the updates that are worth looking at in the owner's Inbox.

## What it does

- Once a day (`ERMITE_DEPS_CHECK_AT`, 06:00 UTC by default) it checks every repository that is not [archived](cli.md#archived-repositories). Archived repositories are skipped.
- It reads Go modules (`go.mod`) and GitHub Actions (`uses:` in workflows and `action.yml`). Vendored modules, `replace`d modules and actions pinned to a commit are left alone.
- For each dependency it proposes the highest version that is older than the cooldown (`ERMITE_DEPS_COOLDOWN`, 72 hours by default) and has no malicious-package (`MAL-`) advisory. A version that cannot be checked is not proposed. Go modules never move to a new major path, and a Go module version is proposed only if its `go.mod` does not need a newer `go` directive than the requiring module's.
- A check that finds a version the previous one did not publishes a `deps.updates_found` event, delivered to the owner only.

`ermite deps check <repo>` runs the same check now. See [Configuration](configuration.md) for the `ERMITE_DEPS_*` settings; `ERMITE_DEPS_ENABLED=0` turns Bernard off.

## How updates are applied

GitHub Actions updates are applied by the server. Go module updates are applied by a runner, through a CI run: Bernard starts a run with the event `dependencies` on the tip of the default branch, with one job, **Update dependencies**. A runner that offers the `deps` job kind and has the label set by `ERMITE_DEPS_RUNNER_LABEL` (`ubuntu-latest` by default) takes it like any other job. The job gets a read-only repository token for that commit, no secrets, no variables and no ID token. The runner runs `go get` and `go mod tidy` for the requested versions and reports the changed `go.mod` and `go.sum` files.

Bernard accepts a report only if it changes `go.mod` and `go.sum` of the requested directories, keeps `replace`, `exclude`, `retract`, `go` and `toolchain` as they were, sets each requested module to exactly the requested version and leaves the other direct requirements alone. It then commits the files, together with the Actions updates, on `bernard/deps` on top of the commit the job ran on, and opens or updates the merge request.

The run shows in the Actions tab, in `ermite run list` (`--workflow bernard/deps` keeps only these runs) and in the Inbox under **Running**, as workflow `Dependencies` by `bernard`. Its log is on the run page and in `ermite run view <repo> <run> --log`. A run:

- can be cancelled (`ermite run cancel`), which fails the update; closing or merging the Bernard merge request cancels a run that is still going.
- posts no check and cannot be re-run: click **Retry** in the Inbox to start a new one.
- has `ERMITE_DEPS_JOB_TIMEOUT` (10 minutes by default) from the moment a runner takes it, as the `timeout-minutes` of the job. The wait for a runner is CI's: a run no runner takes within 24 hours fails.
- survives a server restart. If the server stops after the run ended and before Bernard applied the result, the update fails with a reason and you can retry.

Nothing is retried on its own. A failed update keeps its updates listed with a one-line reason, such as `the run ended: failure`, and a link to the run.

## The Inbox table

**Dependency updates** comes after the running workflows. The header shows when the dependencies were last checked and how many repositories and updates are pending. There is one row per repository with pending updates:

- A disclosure arrow, the repository name, the number of updates, their kinds (`1 major · 2 minor · 3 patch`, empty kinds left out) and a **new** badge for versions that appeared since the last check. Open the row to see each update: name, `current → latest`, kind, **new**, and the directory when it is not the repository root.
- The status: `queued` or `running` with a link to the run while Bernard applies updates, `failed` with a link to the run (the reason is its tooltip), or the link to the open merge request (`MR !7`).
- The main button: **Open MR** opens the Bernard merge request, **Update MR** updates it, **View MR** is a link when it already holds exactly the pending versions, **Retry** starts a new run after a failure. There is no button while a run is going.
- The **⋯** menu holds **Ignore**, which hides exactly the listed versions (a newer version shows up again), and **Snooze**, which hides the repository for seven days (`ERMITE_DEPS_SNOOZE`).

Each button sends the versions you saw. If a check changed the list in the meantime, or an update is being applied, the server refuses and you reload. The table updates live and keeps the rows you opened. Only the owner sees the Inbox and these buttons; everyone else gets a 404.

The same actions are available as `ermite deps list|check|open|ignore|snooze` ([CLI](cli.md#dependency-updates)) and as `DepsService` in the API.
