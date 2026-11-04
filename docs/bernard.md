# Bernard

Bernard watches the dependencies of your repositories and lists the updates that are worth looking at in the owner's Inbox.

## What it does

- Once a day (`ERMITE_DEPS_CHECK_AT`, 06:00 UTC by default) it checks every repository that is not [archived](cli.md#archived-repositories). Archived repositories are skipped.
- It reads Go modules (`go.mod`), GitHub Actions (`uses:` in workflows and `action.yml`) and npm packages (`dependencies` and `devDependencies` of every `package.json` outside `node_modules`, `testdata`, `fixtures` and `__fixtures__`; an invalid `package.json` is skipped with a warning). Vendored modules, `replace`d modules and actions pinned to a commit are left alone.
- For each dependency it proposes the highest version that is older than the cooldown (`ERMITE_DEPS_COOLDOWN`, 72 hours by default) and has no malicious-package (`MAL-`) advisory. A version that cannot be checked is not proposed. A Go module version is proposed only if its `go.mod` does not need a newer `go` directive than the requiring module's and its `module` line is the path Bernard would require (a version that moved to another path, such as a vanity domain, is skipped).
- Go majors include new module paths: `foo` → `foo/v2` and `gopkg.in/x.v1` → `gopkg.in/x.v2` (up to five newer paths, stopping at the first missing one).
- A check that finds a version the previous one did not publishes a `deps.updates_found` event, delivered to the owner only.

`ermite deps check <repo>` runs the same check now. See [Configuration](configuration.md) for the `ERMITE_DEPS_*` settings; `ERMITE_DEPS_ENABLED=0` turns Bernard off.

## Batches and merge requests

Patch and minor updates share the base batch (key `""`), branch `bernard/deps`, and subject `build(deps): update N dependencies`. Newer majors are grouped by family, across directories, in one batch per family keyed `major:<ecosystem>:<family>`: an npm scope (`@scope/`), an Actions owner (`actions/`), for Go `host/owner/` on multi-owner hosts (github.com, gitlab.com, codeberg.org, bitbucket.org, git.bnema.dev, gitea.com, git.sr.ht, salsa.debian.org), `golang.org/x/`, and `host/` on any other (vanity) host such as `go.opentelemetry.io/`. Unscoped npm packages, gopkg.in modules and single-segment names have no family and get a batch of their own. The owner can split a dependency out of its family (`major:<ecosystem>:<name>`) and put it back. Its branch is `bernard/deps-major/<slug>-<hash>` (a batch upgraded from the per-dependency keys of schema 12 keeps its old branch) and its subject is `build(deps): update <name> to <major>` for one package, `build(deps): update <family>* to new majors` for several. The highest eligible same-major and cross-major versions can both be proposed for the same dependency. Floating Actions tags stay floating (`v4` → `v5`); pinned versions stay pinned (`v4.1.0` → `v5.0.0`).

`ermite deps split <repo> <ecosystem> <name>` gives a dependency's majors a batch of their own; `--join` puts them back in their family. It is refused while a batch it changes is applying or queued.

Open and Ignore act independently on each batch: `ermite deps open|ignore <repo> --batch KEY` takes the BATCH column of `ermite deps list`, and `base` (the default) names the base batch. Snooze remains repository-wide. Merging or closing one MR leaves other batches alone; an empty major batch is removed when its MR ends.

The daily check preserves each batch's run and MR state. If a proposal vanishes while a major MR is open or a run is applying, the batch remains with no pending updates. An MR is **outdated** when it holds other versions than the batch now proposes (a newer version in the same major, say) or was built from other current versions (another batch updating the same dependency was merged). It stays open; Open/Update MR rebuilds its branch from the current default-branch tip.

### Automatic merge requests

After each check (daily, after a merge, or `ermite deps check`), Bernard queues the base batch by itself when:

- `ERMITE_DEPS_AUTO_OPEN` is on (the default) and the repository's setting is on (`ermite deps auto <repo> on|off`, or the Inbox toggle);
- the repository is not snoozed;
- the batch has pending updates and no merge request, or an outdated one;
- its branch is not [adopted](#adopted-branches), and it is not queued or applying;
- it did not fail on these same versions: a failed batch is queued again only once its versions change.

Majors are never queued automatically: open them by hand. Turning the repository setting on queues the base batch at once when these conditions hold.

### The queue

Open, Retry and the automatic merge requests put the batch in a queue (`queued`); `deps list` and the Inbox show its position, 1 being next. Bernard takes the batches in order: those opened by hand first, then the oldest.

1. **Server step**, one batch at a time: Bernard writes the draft (see below), or for Actions-only batches the final commit and its merge request. It needs no runner.
2. **Run step**: a `drafted` batch starts its dependency run when fewer than `ERMITE_DEPS_CONCURRENCY` (1 by default) runs are going across all repositories. An automatic batch also waits while any other workflow job waits for a runner, so normal CI goes first; a batch opened by hand does not wait.

The queue is in the store: it resumes after a restart. Bernard looks at it on each Open, at the end of each run, on CI run and job changes, and every minute. When the versions of a batch opened by hand changed while it waited, it fails with `the pending dependency updates changed, reload and retry`; an automatic batch takes the current versions. Closing the batch's merge request or ignoring its updates takes it out of the queue.

### Draft merge requests

A batch with Go or npm updates gets its merge request before the runner works:

1. The server step commits the manifest bump (`go.mod` and `package.json` versions only, no `go.sum` or lockfile) and the Actions updates on the batch branch, on top of the default-branch tip, and opens or updates the merge request as a draft: `WIP: <subject>`.
2. The run updates the dependencies on that tip (see below). Its accepted report becomes a second commit on top of the draft, and the merge request is marked ready with the plain subject.
3. If the run fails, the draft stays. Its description shows the failure and the commands that redo the update locally.

Workflows do not run on the draft commit: its push and `pull_request` runs are held until the dependency run ends. The final commit runs them as any push; when it equals the draft, the held runs start on the draft. After a failure no run starts.

A batch of Actions updates only gets one commit and a ready merge request.

### Adopted branches

A push by anyone but Bernard to a batch branch with an open merge request adopts it. Bernard also adopts a branch it finds moved when it writes. Bernard never rewrites an adopted branch: Open is refused with `the branch was changed by hand`, a run that ends only refreshes the merge request description, and no automatic merge request is queued. `deps list` marks the batch `adopted`. The adoption ends with its merge request (merged or closed).

To finish a failed draft yourself, push the fix to its branch: the merge request becomes yours to merge.

## How updates are applied

GitHub Actions updates are applied by the server. Go module and npm updates are applied by a runner, through a CI run: when the batch gets a run slot, Bernard starts a run with the event `dependencies` on the tip of the default branch, with one job, **Update dependencies**. The job gets a read-only repository token for that commit, no secrets, no variables and no ID token.

A runner takes the job when it has the label set by `ERMITE_DEPS_RUNNER_LABEL` (`ubuntu-latest` by default) and every job kind the batch needs:

| Kind | Needed for |
|---|---|
| `deps` | every runner-applied batch |
| `deps.npm` | npm updates |
| `deps.gomod-path-majors` | Go majors that move to a new module path |

The server learns a runner's kinds when it polls. If no registered runner with the label offers them, the batch fails with `no runner supports <kind> updates` and no run starts.

For Go, the runner runs `go get` and `go mod tidy` for the requested versions. For a new module path it first rewrites the import paths in the module's `.go` files (not `vendor`, `testdata` or nested modules). Go module downloads are kept in a per-repository cache on the runner, separate from workflow caches.

### npm

Each workspace root is updated by its own package manager. A directory is a root when its `package.json` has `workspaces` or it holds `pnpm-workspace.yaml`; a package matched by a root's patterns belongs to that root, any other package is its own root.

- Manager: the lockfile decides: `bun.lock` or `bun.lockb` → Bun, `pnpm-lock.yaml` → pnpm, `yarn.lock` → Yarn, `npm-shrinkwrap.json` (preferred) or `package-lock.json` → npm; no lockfile → npm. The root's `packageManager` field (`npm`, `pnpm`, `yarn` or `bun` with an exact version) sets the version; it must name the tool of the existing lockfile, otherwise the root is skipped. Without `packageManager`, the version comes from the lockfile format; a pnpm lockfile other than version 6 or 9 needs `packageManager`. A root whose `packageManager` is npm older than 7 is skipped. Yarn classic needs 1.22 or later: a root whose `packageManager` names an older Yarn is skipped with a warning, because older Yarn 1 releases run a git dependency's `prepare` script despite `--ignore-scripts`.
- Yarn Berry (Yarn 2 and later: a `yarn.lock` with `__metadata`, a `.yarnrc.yml`, or `packageManager` `yarn@2` or later) is not supported; its roots are skipped with a warning. Berry can run repository code during an install, and blocking each path one by one leaves gaps: `.yarnrc.yml` loads `yarnPath` and plugins, `exec:` dependencies run a script (even inside `file:` tarballs), `portal:` and `file:` reach outside the root, and a GitHub source pinned in `yarn.lock` is fetched as a tarball whose `prepack` script runs.
- Node: the major of the root's `engines.node` (`>=N`, `^N`, `~N`, `N`, `N.x`), clamped to 18–24; 22 otherwise.
- Ranges: `X.Y.Z`, `^X.Y.Z`, `~X.Y.Z`, `>=X.Y.Z` and `=X.Y.Z` are updated with their operator kept (`^1.2.0` → `^1.9.0`). Other specs (`workspace:`, `file:`, git, URLs, tags, `*`, `||`, x-ranges, …) are skipped, and so is a package listed with different specs in both sections. Only the public registry (`registry.npmjs.org`) is read; a package whose registry document exceeds 128 MiB, or whose lookup fails, is skipped. npm lookups share a budget of three minutes per check (reserving a quarter of the remaining check time, up to 30 seconds); once it is spent, the remaining packages are skipped for that check and the other ecosystems' updates are kept. A package skipped this way, or whose lookup fails, keeps the updates already proposed for it, provided they are not ignored and the malicious-package check of each still answers no (no answer drops it). When only the malicious-package check of a new version gets no answer, the update already proposed in that lane (same major or new major) is kept. The daily check and the check after a merge have five minutes per repository, a check from the CLI or web one minute.

No repository code runs during the job:

- lifecycle scripts are disabled for every manager;
- pnpm ignores `.pnpmfile.cjs`;
- Yarn loads no rc file from the repository (`yarnPath`, `yarn-path`);
- Bun ignores the repository's `bunfig.toml` and refuses git dependencies.

The job's network allows the internet except private address ranges. Protection relies on the disabled scripts and on the server checking the report.

### The report

The runner reports at most 64 changed files, 2 MiB per file and 8 MiB in total. Bernard accepts it only if every file is one an ecosystem allows:

- Go: `go.mod` and `go.sum` of the requested directories, and for a new module path the `.go` files of that module, where only import paths from the old to the new path may change. A changed `.go` file larger than 2 MiB makes the report fail.
- npm: the `package.json` of each requested directory, byte for byte the original with only the requested specs changed, and the root's lockfile if it existed before. In `package-lock.json` and `npm-shrinkwrap.json`, every new or changed `resolved` entry must start with `https://registry.npmjs.org/`; when the original lockfile is over 2 MiB, Bernard reads only its header, so every resolution counts as new. pnpm, Yarn and Bun lockfiles are not checked: review them in the merge request.

For `go.mod`:

- `replace`, `exclude`, `retract`, `go` and `toolchain` are unchanged;
- each requested module is at the requested version or higher when Go selects it, or removed when it was indirect and no longer needed;
- other direct requirements may only be raised by Go, never lowered, removed, added or made indirect;
- no `require` line is duplicated.

The server checks the report when the job ends: a refused report fails the job and the run, with the reason in the log and in the Inbox. Otherwise the commit's tree is the job's commit with the reported files and the Actions updates, its parent the draft commit. The merge request lists the versions Go raised or removed under "Also changed by Go".

The run shows in the Actions tab, in `ermite run list` (`--workflow bernard/deps` keeps only these runs) and in the Inbox under **Running**, as workflow `Dependencies` by `bernard`. Its log is on the run page and in `ermite run view <repo> <run> --log`. A run:

- can be cancelled (`ermite run cancel`), which fails the update; closing the Bernard merge request cancels a run that is still going.
- posts no check and cannot be re-run: **Retry** in the Inbox (or `ermite deps open`) queues the batch again.
- has `ERMITE_DEPS_JOB_TIMEOUT` (10 minutes by default) from the moment a runner takes it, as the `timeout-minutes` of the job. The wait for a runner is CI's: a run no runner takes within 24 hours fails.
- survives a server restart. If the server stops after the run ended and before Bernard applied the result, the update fails with a reason and you can retry.

A failed update keeps its updates listed with a bounded failure reason, such as `the run ended: failure`, a link to the run and the commands that redo it locally. It is queued again on its own only as an automatic merge request with other versions.

## The Inbox table

**Dependency updates** lists the repositories with pending updates. The header shows when the dependencies were last checked, how many batches and updates are pending and, while Bernard works, the queue (`queue 1/1 running, 12 waiting`). The chips **All**, **Failures**, **Majors** and **MRs** filter the batches (`?filter=`); **sort** puts the repositories with a failure first, then the most updates, or orders them by name.

There is one collapsible row per repository, closed unless it holds a failure. Its line tells the failures, the number of patch and minor updates with the state of their batch, the number of major families and a **new** badge for versions that appeared since the last check. Open it to see:

- **Patch & minor**, the batch Bernard opens on its own as a draft merge request: its status (`queued #3`, `drafting`, `running` with a link to the run, `MR !41` with **draft** until its run passes, **adopted** once someone pushed to the branch, or `failed`) and its button.
- **Majors**, one row per family (`github.com/charmbracelet/*  bubbles, lipgloss → v2`) or lone package (`go-acme/lego → v5`), each with its status and button.
- A failure shows the excerpt of the output Bernard keeps, the commands that reproduce it locally, and a link to the log for the full output.

Open any batch row to see each update: name, `current → latest`, kind, **new**, and the directory when it is not the repository root. An **outdated** badge marks a merge request that holds other versions.

The buttons: **Open MR** opens the Bernard merge request, **Update MR** updates it, **View MR** is a link when it already holds exactly the pending versions (or was adopted), **Retry** starts a new run after a failure. There is no button while a batch is queued or running. An adopted batch only offers **View MR**, whatever its state: Bernard no longer writes its branch, and a failure stays shown with a note that the branch is in human hands. The **⋯** menu of a batch holds **Ignore**, which hides exactly the listed versions (a newer version shows up again), **Split <package>** to give a package of a family its own merge request, and **Join family** to put it back. The repository row has **Re-check**, which detects its updates again, and a **⋯** menu with **Auto-open on/off** (whether Bernard opens the patch and minor merge request by itself) and **Snooze**, which hides the repository for seven days (`ERMITE_DEPS_SNOOZE`).

**Open MR**, **Update MR**, **Retry** and **Ignore** send the versions you saw: if a check changed the list in the meantime, or an update is being applied, the server refuses and you reload. Each action and each event redraws the table (header, chips, order), and the rows you opened stay open. Only the owner sees the Inbox and these buttons; everyone else gets a 404.

The same actions are available as `ermite deps list|check|open|ignore|snooze|auto|split` ([CLI](cli.md#dependency-updates)) and as `DepsService` in the API.
