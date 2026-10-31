# Bernard

Bernard watches the dependencies of your repositories and lists the updates that are worth looking at in the owner's Inbox.

## What it does

- Once a day (`ERMITE_DEPS_CHECK_AT`, 06:00 UTC by default) it checks every repository that is not [archived](cli.md#archived-repositories). Archived repositories are skipped.
- It reads Go modules (`go.mod`), GitHub Actions (`uses:` in workflows and `action.yml`) and npm packages (`dependencies` and `devDependencies` of every `package.json` outside `node_modules`, `testdata`, `fixtures` and `__fixtures__`; an invalid `package.json` is skipped with a warning). Vendored modules, `replace`d modules and actions pinned to a commit are left alone.
- For each dependency it proposes the highest version that is older than the cooldown (`ERMITE_DEPS_COOLDOWN`, 72 hours by default) and has no malicious-package (`MAL-`) advisory. A version that cannot be checked is not proposed. A Go module version is proposed only if its `go.mod` does not need a newer `go` directive than the requiring module's.
- Go majors include new module paths: `foo` → `foo/v2` and `gopkg.in/x.v1` → `gopkg.in/x.v2` (up to five newer paths, stopping at the first missing one).
- A check that finds a version the previous one did not publishes a `deps.updates_found` event, delivered to the owner only.

`ermite deps check <repo>` runs the same check now. See [Configuration](configuration.md) for the `ERMITE_DEPS_*` settings; `ERMITE_DEPS_ENABLED=0` turns Bernard off.

## Batches and merge requests

Patch and minor updates share the base batch (key `""`), branch `bernard/deps`, and subject `build(deps): update N dependencies`. Each dependency with a newer major has a separate batch, keyed by ecosystem, directory, current name and major. Its branch is `bernard/deps-major/<slug>-<hash>` and its subject is `build(deps): update <name> to <major>`. The highest eligible same-major and cross-major versions can both be proposed for the same dependency. Floating Actions tags stay floating (`v4` → `v5`); pinned versions stay pinned (`v4.1.0` → `v5.0.0`).

Open and Ignore act independently on each batch: `ermite deps open|ignore <repo> --batch KEY` takes the BATCH column of `ermite deps list`, and `base` (the default) names the base batch. Snooze remains repository-wide. Merging or closing one MR leaves other batches alone; an empty major batch is removed when its MR ends.

The daily check preserves each batch's run and MR state. If a proposal vanishes while a major MR is open or a run is applying, the batch remains with no pending updates. An MR is **outdated** when it holds other versions than the batch now proposes (a newer version in the same major, say) or was built from other current versions (another batch updating the same dependency was merged). It stays open; Open/Update MR rebuilds its branch from the current default-branch tip. No MR is opened or refreshed automatically.

## How updates are applied

GitHub Actions updates are applied by the server. Go module and npm updates are applied by a runner, through a CI run: Bernard starts a run with the event `dependencies` on the tip of the default branch, with one job, **Update dependencies**. The job gets a read-only repository token for that commit, no secrets, no variables and no ID token.

A runner takes the job when it has the label set by `ERMITE_DEPS_RUNNER_LABEL` (`ubuntu-latest` by default) and every job kind the batch needs:

| Kind | Needed for |
|---|---|
| `deps` | every runner-applied batch |
| `deps.npm` | npm updates |
| `deps.gomod-path-majors` | Go majors that move to a new module path |

The server learns a runner's kinds when it polls. If no registered runner with the label offers them, Open fails with `no runner supports <kind> updates` and starts no run.

For Go, the runner runs `go get` and `go mod tidy` for the requested versions. For a new module path it first rewrites the import paths in the module's `.go` files (not `vendor`, `testdata` or nested modules). Go module downloads are kept in a per-repository cache on the runner, separate from workflow caches.

### npm

Each workspace root is updated by its own package manager. A directory is a root when its `package.json` has `workspaces` or it holds `pnpm-workspace.yaml`; a package matched by a root's patterns belongs to that root, any other package is its own root.

- Manager: the lockfile decides: `bun.lock` or `bun.lockb` → Bun, `pnpm-lock.yaml` → pnpm, `yarn.lock` → Yarn, `npm-shrinkwrap.json` (preferred) or `package-lock.json` → npm; no lockfile → npm. The root's `packageManager` field (`npm`, `pnpm`, `yarn` or `bun` with an exact version) sets the version; it must name the tool of the existing lockfile, otherwise the root is skipped. Without `packageManager`, the version comes from the lockfile format; a pnpm lockfile other than version 6 or 9 needs `packageManager`. A root whose `packageManager` is npm older than 7 is skipped. Yarn Berry (Yarn 2 and later: a `yarn.lock` with `__metadata`, a `.yarnrc.yml`, or `packageManager` `yarn@2` or later) is not supported: its roots are skipped with a warning.
- Node: the major of the root's `engines.node` (`>=N`, `^N`, `~N`, `N`, `N.x`), clamped to 18–24; 22 otherwise.
- Ranges: `X.Y.Z`, `^X.Y.Z`, `~X.Y.Z`, `>=X.Y.Z` and `=X.Y.Z` are updated with their operator kept (`^1.2.0` → `^1.9.0`). Other specs (`workspace:`, `file:`, git, URLs, tags, `*`, `||`, x-ranges, …) are skipped, and so is a package listed with different specs in both sections. Only the public registry (`registry.npmjs.org`) is read; a package whose registry document exceeds 128 MiB, or whose lookup fails, is skipped. npm lookups share a budget of three minutes per check (less when the check has less time left); once it is spent, the remaining packages are skipped for that check and the other ecosystems' updates are kept. The daily check and the check after a merge have five minutes per repository, a check from the CLI or web one minute.

No repository code runs during the job:

- lifecycle scripts are disabled for every manager;
- pnpm ignores `.pnpmfile.cjs`;
- Yarn loads no rc file from the repository (`yarnPath`, `yarn-path`);
- Bun ignores the repository's `bunfig.toml` and refuses git dependencies.

The job's network allows the internet except private address ranges. Protection relies on the disabled scripts and on the server checking the report.

### The report

The runner reports at most 64 changed files, 2 MiB per file and 8 MiB in total. Bernard accepts it only if every file is one an ecosystem allows:

- Go: `go.mod` and `go.sum` of the requested directories, and for a new module path the `.go` files of that module, where only import paths from the old to the new path may change.
- npm: the `package.json` of each requested directory, byte for byte the original with only the requested specs changed, and the root's lockfile if it existed before. In `package-lock.json` and `npm-shrinkwrap.json`, every new or changed `resolved` entry must start with `https://registry.npmjs.org/`; when the original lockfile is over 2 MiB, Bernard reads only its header, so every resolution counts as new. pnpm, Yarn and Bun lockfiles are not checked: review them in the merge request.

For `go.mod`:

- `replace`, `exclude`, `retract`, `go` and `toolchain` are unchanged;
- each requested module is at the requested version or higher when Go selects it, or removed when it was indirect and no longer needed;
- other direct requirements may only be raised by Go, never lowered, removed, added or made indirect;
- no `require` line is duplicated.

The server checks the report when the job ends: a refused report fails the job and the run, with the reason in the log and in the Inbox. Otherwise Bernard commits the files, together with the Actions updates, on the batch branch on top of the commit the job ran on, and opens or updates the merge request. The merge request lists the versions Go raised or removed under "Also changed by Go".

The run shows in the Actions tab, in `ermite run list` (`--workflow bernard/deps` keeps only these runs) and in the Inbox under **Running**, as workflow `Dependencies` by `bernard`. Its log is on the run page and in `ermite run view <repo> <run> --log`. A run:

- can be cancelled (`ermite run cancel`), which fails the update; closing the Bernard merge request cancels a run that is still going.
- posts no check and cannot be re-run: click **Retry** in the Inbox to start a new one.
- has `ERMITE_DEPS_JOB_TIMEOUT` (10 minutes by default) from the moment a runner takes it, as the `timeout-minutes` of the job. The wait for a runner is CI's: a run no runner takes within 24 hours fails.
- survives a server restart. If the server stops after the run ended and before Bernard applied the result, the update fails with a reason and you can retry.

Nothing is retried on its own. A failed update keeps its updates listed with a bounded failure reason, such as `the run ended: failure`, and a link to the run.

## The Inbox table

**Dependency updates** comes after the running workflows. The header shows when the dependencies were last checked and how many batches and updates are pending. There is one row per batch:

- A disclosure arrow, the repository name, the batch (**Updates** for base, `<name> → <major>` for a major), an **outdated** badge when its merge request holds other versions or was built from other current versions, the number of updates, their kinds (`1 major · 2 minor · 3 patch`, empty kinds left out) and a **new** badge for versions that appeared since the last check. Open the row to see each update: name, `current → latest`, kind, **new**, and the directory when it is not the repository root.
- The status: `queued` or `running` with a link to the run while Bernard applies updates, `failed` with a link to the run (the Inbox shows the failure’s last output line; expand the row for the full excerpt), or the link to the open merge request (`MR !7`).
- The main button: **Open MR** opens the Bernard merge request, **Update MR** updates it, **View MR** is a link when it already holds exactly the pending versions, **Retry** starts a new run after a failure. There is no button while a run is going.
- The **⋯** menu holds **Ignore**, which hides exactly the listed versions (a newer version shows up again), and **Snooze**, which hides the repository for seven days (`ERMITE_DEPS_SNOOZE`).

Each button sends the versions you saw. If a check changed the list in the meantime, or an update is being applied, the server refuses and you reload. The table updates live and keeps the rows you opened. Only the owner sees the Inbox and these buttons; everyone else gets a 404.

The same actions are available as `ermite deps list|check|open|ignore|snooze` ([CLI](cli.md#dependency-updates)) and as `DepsService` in the API.
