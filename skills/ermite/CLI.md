# ermite CLI reference

`ermite <cmd> --help` is authoritative and lists every JSON field. Most commands take `--json`. Errors: one line `ermite: ...` on stderr, exit 1.

## Connection and config

Order: `ERMITE_URL` / `ERMITE_TOKEN` env, then `url` / `token.cmd` in `~/.config/ermite/config` (platform config dir on macOS/Windows; or `$ERMITE_CONFIG`).

- `token.cmd` is a shell command that prints the token (any secret manager). It runs at most once per command, only for the server of the file: with another `ERMITE_URL`, set `ERMITE_TOKEN`. It never runs for `version`, `config` or completion, nor when the file or its dir is writable by others.
- Agents: use `ERMITE_TOKEN` or your own `ERMITE_CONFIG`. Never edit the owner's file, run its `token.cmd` or print its output.

| Command | Notes |
|---|---|
| `config show [--json]` | path, `url`, `token_cmd`; never runs `token.cmd`; warns on bad permissions |
| `config set <url\|token.cmd> <value>` | creates the file (0600), keeps comments; `token.cmd` stored as is |
| `config unset <url\|token.cmd>` | |
| `version [--json]` | client version; with a URL, server version, `features`, `missing_features` |
| `completion bash\|zsh\|fish` | completes repo names (anonymous unless `ERMITE_TOKEN`) |

## Repositories

| Command | Scope | Notes |
|---|---|---|
| `repo list [--json]` | none | public + private you can read |
| `repo show <name> [--json]` | none | |
| `repo create <name> [--description T] [--topics a,b] [--visibility public\|private]` | `repo:write` | private by default; names `[a-z0-9._-]`, 1-100 |
| `repo edit <name> [flags]` | `repo:admin` | only given flags change |
| `repo delete <name> [-y]` | `repo:admin` | without `-y`, type the name; moved to server trash, no undelete |

`repo edit` flags:
- `--description T` / `--topics a,b`: `""` clears (topics: a-z, 0-9, `-`, 1-50 chars, max 20).
- `--visibility public|private` and `--default-branch B` (the branch must exist).
- `--merge-modes fast-forward,squash,merge`: list the modes to keep (all allowed by default).
- `--allow-auto-merge[=false]`.
- `--issues off|owner|public`: `public` adds a moderated form for visitors.
- `--archived[=false]` (`repos.archived` feature): read-only. Blocks pushes, MR/issue writes, merges, release writes, dispatches, reruns and crons; reads and closing still work.

JSON: `name`, `description`, `visibility`, `default_branch`, `created_at`, `pushed_at` (null before first push), `merge_modes`, `allow_auto_merge`, `issues`, `archived`.

## Tokens

| Command | Scope | Notes |
|---|---|---|
| `token create <label> [--preset P] [--scope S]… [--repo R]… [--branch-prefix P/]… [--expires 30d\|2w\|12h\|2027-01-31] [--json]` | `token:admin` | secret printed once on stdout; no default scope; never expires by default |
| `token list [--json]` | `token:admin` | no secrets; admin token not listed |
| `token revoke <id>` | `token:admin` | revoking an owner-rights token also signs every browser out |

Presets (add up with `--scope`): `cli` every scope, `mr:force-merge` included (owner's daily use); `agent` `repo:write`, `mr:write` (no verdicts, no force-merge); `ci` `repo:read`; `runner` `runner:register`.

| Scope | Allows |
|---|---|
| `repo:read` | read private repos |
| `repo:write` | push, create repos |
| `repo:admin` | delete, `repo edit`, `workflow enable/disable` |
| `mr:write` | open, comment, reply, edit/delete own notes, resolve, close, mark read, ready |
| `mr:review` | verdicts |
| `mr:merge` | merge, `--delete-branch`, `--auto` |
| `mr:force-merge` | `--ignore-checks` |
| `issue:write` | create, comment, edit, close, resolve, reopen issues |
| `release:write` | release writes (refused for branch-limited tokens) |
| `journal:read` | `journal`, `release pending` |
| `ci:write` | `run cancel/rerun`, `workflow run` |
| `secrets:write` | secrets, variables, environments |
| `runner:register` | runner registration, `runner list/remove` (not repo-limited) |
| `token:admin` | tokens |

Rules:
- A token never grants more than its creator. `--repo` limits repos (default all). Labels up to 100 bytes; `owner`/`root` reserved.
- `--branch-prefix` (up to 8, ending with `/`) limits writes to `refs/heads/<prefix>…`, where it may push, force-push and delete. Any other ref (`main`, tags, notes) refuses the whole push, naming the ref. Such a token cannot create/delete repos, edit settings, manage secrets or get owner rights; it merges only into its prefixes and creates tokens only within them.
- Owner rights = admin token, or every scope with no `--repo` and no `--branch-prefix`.
- Agent token: `ermite token create claude --preset agent --scope repo:read --repo <repo> --branch-prefix agent/claude/`.

JSON: `id`, `label`, `scopes`, `repos`, `branch_prefixes`, `expires_at`, `created_at`, `last_used_at`.

## Merge requests

Named by repo and number (`mr show proj 3`). Needs the `merge_requests` feature.

| Command | Scope | Notes |
|---|---|---|
| `mr create <repo> --source B --title T [--target B] [--description-file F\|-]` | `mr:write` | target = default branch; title ≤120; description only from file (1 MiB); refused if an open MR has the same source/target; prints URL |
| `mr list <repo> [--state open\|merged\|closed\|all]` | none | default `open` |
| `mr show <repo> <n> [--diff] [--since-last-read] [--mark-read]` | `mr:write` for `--mark-read` | commits, files, comments, linked issues, mergeability; `--diff` gives line numbers for comments |
| `mr comment <repo> <n> -m TEXT\|- [--path P --line N\|A-B --side old\|new] [--reply ID] [--approve\|--request-changes]` | `mr:write`; verdicts `mr:review` | Markdown ≤8000; line must be in the diff; deleted file uses old path; `--reply` takes `12`, `#12` or `^12`, no verdict; a verdict needs no message |
| `mr edit <repo> <n> [--title T] [--description-file F\|-]` | `mr:write` | at least one flag; open or closed, not merged; empty file clears |
| `mr edit-comment <repo> <n> <id> -m TEXT\|-` | `mr:write` | own notes, open MR; verdict kept |
| `mr delete-comment <repo> <n> <id>` | `mr:write` | own notes; verdict kept |
| `mr resolve <repo> <n> <id> [--undo]` | `mr:write` | on a thread root; idempotent |
| `mr ready <repo> <n>` | `mr:write` | drops `WIP:`/`[WIP]`; open drafts only |
| `mr close <repo> <n>` | `mr:write` | cannot be reopened; prints URL |
| `mr merge <repo> <n> --head <full-id> [--mode M] [--delete-branch] [--ignore-checks]` | `mr:merge` | see below |
| `mr merge <repo> <n> --head <id> --auto [--mode M]` / `--cancel-auto` | `mr:merge` | needs CI and `repo edit --allow-auto-merge` |

Merging:
- **Modes.** `fast-forward` (default; target must be an ancestor of the head), `squash` (one commit, target as parent) or `merge` (two parents). Squash and merge must be conflict-free and are signed by the server key. The server never rebases.
- **`--head`.** Must equal the current source head, else the merge is refused: review again. The command answers once the target moved.
- **Checks.** None failed, queued or running; a head with no checks can merge. `--ignore-checks` needs `mr:force-merge` and cannot be combined with `--auto` or `--cancel-auto`.
- **`--delete-branch`.** Deletes the source after the merge. Refused for the default branch or a branch used by another open MR. JSON adds `source_branch_deleted`.
- **`--auto`.** The server merges as you once CI settles with at least one passed check and none failing; failing checks or conflicts just wait. Cancelled by a new push, a deleted source, a disallowed mode, the setting turned off or lost merge rights. A manual merge supersedes it (`auto=false`, no cancel event); a close cancels it (reason `closed`). JSON `auto_merge` is null unless pending.
- **Mergeability.** `state` `up_to_date`, `behind` (`behind`, `behind_capped`; use squash/merge), `conflict` (`conflict_paths`, `conflict_paths_truncated`) or `checking`. JSON `mergeability` is null unless open.

Verdicts: the latest per author counts. Approvals go stale on a new head; changes requested never do. Verdicts never block a merge. `^N` in notes refers to note N.

JSON: `repo`, `url`, `number`, `title`, `description`, `source_branch`, `target_branch`, `state`, `author`, `created_at`, `updated_at`, `head_id`, `merged_commit_id`, `merge_mode`, `auto_merge`, `auto_merged`. `show` adds `commits`, `diff_base_id`, `stats`, `files` (+`hunks` with `--diff`), `comments`, `verdicts`, `issues`, `issue_events`, `last_read_id`, `last_read_lost`, `mergeability`. Comment: `id`, `author`, `path`, `line`, `start_line`, `side`, `commit_id`, `text`, `resolved`, `reply_to`, `verdict` (`""`, `approve`, `changes_requested`), `created_at`, `edited_at`, `deleted`.

## Issues

They share numbering with MRs. Enable them with `repo edit <repo> --issues owner|public` (default `off`). Needs the `issues` feature. Reads need a public repo or `repo:read`; `issue:write` alone does not grant reading. Writes print a confirmation, then the issue URL.

| Command | Scope | Notes |
|---|---|---|
| `issue create <repo> --title T [--body-file F\|-]` | `issue:write` | title ≤120, body Markdown ≤10000 only from file |
| `issue list <repo> [--state open\|closed\|resolved\|all]` | read | default `open` |
| `issue show <repo> <n>` | read | body, referencing MRs, comments, history; `resolved by !N` |
| `issue comment <repo> <n> -m TEXT\|-` | `issue:write` | plain text ≤10000 |
| `issue edit <repo> <n> [--title T] [--body-file F\|-]` | `issue:write` | at least one flag; empty file clears; pending issues refused |
| `issue close` / `resolve` / `reopen <repo> <n>` | `issue:write` | close = not fixed; resolve = fixed (`issues.resolve` feature); reopen works on both |

Merging an MR whose text names `Fixes #N` resolves issue N. History survives removed references. Edits with identical values keep `updated_at`.

JSON: `repo`, `url`, `number`, `title`, `body`, `state`, `author`, `created_at`, `updated_at`, `comment_count`, `closed_by_merge_request`. `show` adds `comments` (`id`, `author`, `text`, `created_at`), `references` (`number`, `title`, `state`, `closes`) and `events` (`id`, `kind` referenced|closed|reopened|resolved|edited, `issue`, `merge_request`, `merge_request_title`, `closes`, `actor`, `created_at`, `changed_fields`). `mr show` gives `issues` and `issue_events` with the same fields.

## Releases

Needs the `releases` feature. Reads follow repo visibility; drafts need write access. Writes need `release:write` (not branch-limited) or a CI job with `contents: write`. Select a release by tag or id; `view` and `download` also accept `latest`.

| Command | Notes |
|---|---|
| `release create <repo> <tag> [--title T] [--notes T\|--notes-file F\|-] [--target REF] [--draft] [--prerelease] [--latest] [--asset FILE[#LABEL]]…` | existing tags never move; `--target` creates a missing tag |
| `release list <repo> [--limit 30]` | newest first |
| `release view <repo> <tag\|id\|latest> [--notes]` | `--notes` prints only Markdown |
| `release edit <repo> <tag\|id> [--title] [--notes\|--notes-file] [--tag T] [--target] [--draft\|--publish] [--prerelease\|--no-prerelease] [--latest]` | only given fields change |
| `release delete <repo> <tag\|id> [-y]` | keeps the Git tag |
| `release upload <repo> <tag\|id> FILE[#LABEL]… [--clobber] [--content-type T]` | |
| `release download <repo> <tag\|id\|latest> [--name N]… [--pattern GLOB]… [-D dir]` | never overwrites |
| `release pending [repo]` | non-archived repos with commits after their nearest tag (≤500); owner or `journal:read` |

## CI runs and workflows

Workflows are read from `.github/workflows`. Reading runs needs repo read access. Artifacts always need a token, even on public repos.

| Command | Scope | Notes |
|---|---|---|
| `run list <repo> [--workflow F] [--branch B] [--status queued\|in_progress\|completed] [--limit 20]` | read | Bernard runs: `--workflow bernard/deps` |
| `run view <repo> <id> [--attempt N] [--log\|--summary] [--job ID\|NAME]` | read | jobs, steps, annotations; `--log` raw job logs; `--summary` step-summary Markdown |
| `run watch <repo> <id> [--log] [--interval 2s]` | read | exit 0 on success, 1 otherwise |
| `run cancel <repo> <id>` | `ci:write` | |
| `run rerun <repo> <id> [--failed]` | `ci:write` | new attempt; `--failed` only failed jobs and their dependents |
| `run artifacts <repo> <id>` | token | list, with `used_bytes`/`quota_bytes` |
| `run download <repo> <id> [-n NAME]… [-D dir]` | token | zips extracted; never overwrites |
| `workflow list <repo>` | `repo:read` if private | files on the default branch + disabled ones it lacks |
| `workflow disable <repo> <file>` | `repo:admin` | no event (push, PR, schedule, workflow_run, create, delete, dispatch) starts it; started runs go on; file need not exist (disable before first push); idempotent |
| `workflow enable <repo> <file>` | `repo:admin` | missed events not replayed |
| `workflow run <file> -R <repo> [--ref REF] [-f name=value]…` | `ci:write` | needs a `workflow_dispatch` trigger; refused while disabled |

`<file>` is `release.yml` or `.github/workflows/release.yml`. Branch-limited tokens cannot enable or disable workflows. Workflow JSON: `repo`, `file`, `name`, `state` (`enabled`/`disabled`).

## Secrets, variables, environments

These follow GitHub semantics: `${{ secrets.NAME }}` and `${{ vars.NAME }}`. Each lives in a repo (`-R`), a repo environment (`-R R -e ENV`) or the instance (`--instance`). Precedence: environment > repo > instance. Names are letters, digits and `_`, stored uppercase, cannot start with a digit or `GITHUB_`; values ≤48 KiB.

Writes need `secrets:write`. `--instance` needs a token without `--repo`. Branch-limited tokens are refused, and their runs get no secrets. The server needs `ERMITE_SECRETS_KEY`.

| Command | Notes |
|---|---|
| `secret set [NAME] -R R [-e ENV\|--instance] [-b VALUE \| < file \| -f .env]` | stdin preferred (`-b` shows in `ps`); terminal prompts silently; `-f` reads `NAME=value` lines |
| `secret list -R R [-e ENV\|--instance]` | names + `updated_at` of that scope only; values never shown |
| `secret delete NAME -R R [-e ENV\|--instance]` | |
| `variable set [NAME] … [-b VALUE \| stdin \| -f file]` | same scopes; readable, may appear in logs |
| `variable get NAME …` | prints only the value |
| `variable list …` / `variable delete NAME …` | |
| `environment create NAME -R R` | idempotent; a job's `environment:` also creates it |
| `environment list -R R` | |
| `environment delete NAME -R R` | deletes its secrets and variables too |

## Runners and OIDC

| Command | Notes |
|---|---|
| `runner list` | `runner:register`; `isolation`, `labels`, `online` (seen in last 2 min), `busy` |
| `runner remove <id>` | token stops at once; a running job ends or fails as lost |
| `oidc rotate-key` | owner; old key stays in JWKS for 24 h; needs `ci.oidc` feature and `ERMITE_SECRETS_KEY` |

Jobs with `permissions: id-token: write` get OIDC tokens. The issuer is `<public URL>/-/oidc`.

## Mirrors

These push to a remote over SSH. Pushes are atomic and never forced or deleting. If the remote refuses any ref, the mirror pauses as diverged. The server needs `ERMITE_SECRETS_KEY`.

| Command | Notes |
|---|---|
| `mirror key show` | `authorized_keys` line, e.g. `\| gh ssh-key add - --title "ermite mirror"` |
| `mirror key rotate` | owner; every mirror fails until the new key is on the remotes |
| `mirror config [--url 'git@host:org/{repo}.git'] [--cron '*/15 * * * *'] [--all-branches] [--enabled]` | owner; global policy; UTC cron, ≥5 min apart |
| `mirror config show` | owner |
| `mirror add <repo> <url> [--cron C] [--all-branches]` | per-repo override, works with the policy off |
| `mirror exclude <repo>` / `mirror include <repo>` | leave / rejoin the global policy |
| `mirror remove <repo>` | drop the override |
| `mirror status [repo]` | all repos = owner; `state` disabled, ok, failing, paused |
| `mirror sync <repo>\|--all [--force]` | waits ~8 s, then reports queued; `--force` skips the no-op check and throttle, lifts a pause, never force-pushes |

By default the default branch and all tags are pushed; `--all-branches` pushes every branch.

## Bernard (dependency updates, owner only)

Bernard checks daily for updates to Go modules and GitHub Actions. It skips archived repos and versions younger than the cooldown. Go dependency jobs share a per-repository module cache isolated from workflow caches.

| Command | Notes |
|---|---|
| `deps list [--repo R]` | pending updates, snoozed included |
| `deps check <repo>` | check now |
| `deps open <repo>` | open/update the MR on `bernard/deps` |
| `deps ignore <repo>` | ignore the pending versions |
| `deps snooze <repo>` | hide for `ERMITE_DEPS_SNOOZE` |

`open` and `ignore` send back the list they read and are refused if it changed meanwhile. Go updates accept higher versions selected by Go and requested indirect modules removed by tidy; the MR lists these side effects. A report refused by the server fails the job and run before publication. Failed run logs: `run view <repo> <run_id> --log`.

## Activity journal

`journal [--since 24h|7d|2w|2026-10-01|RFC3339] [--repo R] [--kind K,…] [--limit 100]` lists entries newest first, kept 30 days. Kinds: `mr_opened`, `mr_merged`, `mr_closed`, `issue_opened`, `issue_closed`, `tag_created`, `tag_deleted`, `release_published`, `push` (default branch). It needs owner rights or `journal:read` and the `journal` feature. Use it to catch up since your last session. JSON: `id`, `kind`, `repo`, `at`, `actor`, `number`, `ref`, `title`, `commit_count` (≤100), `commits_capped`, `commit`, `summary`.

## Events

`watch [repo] [--json]` runs until interrupted, reconnects and resumes after the last event. `--json` prints NDJSON with `id`, `time`, `type` and the fields below.

| Type | Fields |
|---|---|
| `repo_created`, `repo_updated`, `repo_deleted` | `repo`, `repository` |
| `pushed` | `repo`, `ref_updates` (`ref`, `old_id`, `new_id`; empty = created/deleted) |
| `merge_request_opened`, `_merged`, `_closed` | `repo`, `merge_request`; merged has `auto` (true for an auto-merge) |
| `merge_request_auto_merge_enabled`, `_cancelled` | `repo`, `merge_request`; cancelled has `reason` |
| `merge_request_mergeability` | `repo`, `number`, `mergeability` (on change) |
| `comment_added`, `_edited`, `_deleted`, `_resolved` | `repo`, `number`, `comment` (`verdict` set on verdict notes) |
| `issue_edited`, `merge_request_edited` | `repo`, `number`, `changed_fields` |
| `gap` | events were lost: reload the state |

Unknown types are skipped.

## Signing keys

| Command | Notes |
|---|---|
| `signing-key show [--public-key\|--allowed-signers]` | server key that signs squash/merge commits; no token needed |
| `signing-key trust list` | trusted keys, builtin first |
| `signing-key trust add --format ssh\|gpg --title T --email E… <file\|->` | owner; 1-10 emails |
| `signing-key trust remove <id>` | owner; builtin key cannot be removed |

A pushed commit shows verified when a trusted key signs it for the committer's email. To verify locally: `signing-key show --allowed-signers >> ~/.config/git/allowed_signers`, then `git config gpg.ssh.allowedSignersFile` to that file.

## Web sign-in

These need owner rights.

| Command | Notes |
|---|---|
| `web login` | one-time link (~1 min, 12 h session); it is a secret, never paste it |
| `web logout --all` | ends every browser session; tokens unaffected |
| `web passkey list` | `id`, `name`, `created_at`, `last_used_at`, `synced` |
| `web passkey remove <id>` | also signs every browser out |

Passkeys need an https (or localhost) public URL with a host name, not an IP; otherwise `passkey list` fails with "passkeys are not available on this server". Adding one needs a browser at `/-/passkeys`. Agents do not use passkeys.

## Server

`serve` is configured by env only:
- Required: `ERMITE_DATA_DIR`, `ERMITE_PUBLIC_URL`, `ERMITE_ADMIN_TOKEN` (≥32 chars).
- Common: `ERMITE_ADDR` (`:7341`), `ERMITE_TRUSTED_PROXIES`, `ERMITE_OWNER`, `ERMITE_GIT` (git ≥2.45), `ERMITE_GIT_READER` (`cli` if reads look wrong), `ERMITE_SECRETS_KEY`, `ERMITE_LOG_LEVEL`, `ERMITE_LOG_FORMAT`, `ERMITE_COMMITTER_NAME`/`_EMAIL`.
- Families: `ERMITE_CI_*`, `ERMITE_DEPS_*`, `ERMITE_S3_*`, `ERMITE_RELEASE_*`.

Full list: `docs/configuration.md` in the ermite repo.
