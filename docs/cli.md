# CLI

The `ermite` binary also drives a running server over its API. Run `ermite <command> --help` for the details of each command.

## Connect to a server

| Setting | Environment | Config file key |
|---|---|---|
| server address | `ERMITE_URL` | `url` |
| token (optional for public reads) | `ERMITE_TOKEN` | `token.cmd`, a command that prints it |

The environment wins over the config file.

```sh
ermite config set url https://git.example.com
ermite config set token.cmd 'pass show ermite/token'
ermite config show [--json]   # path and values
ermite config unset <key>
```

The file holds no secret, only the command that fetches the token (`pass`, `gopass show -o`, `op read`, `secret-tool lookup`, …):

```ini
# ~/.config/ermite/config
url       = https://git.example.com
token.cmd = pass show ermite/token
```

Location: `$XDG_CONFIG_HOME/ermite/config`, `~/Library/Application Support/ermite/config` on macOS, `%AppData%\ermite\config` on Windows. `ERMITE_CONFIG` names another file.

A token sent over plain `http://` to a non-loopback host prints a warning.

### How `token.cmd` runs

- Through `sh -c` (`cmd /C` on Windows), at most once per run, only when a command needs the token.
- It gets your terminal when stdin is one, so pinentry or biometric prompts work.
- It must print the token alone on one line, under 4 KiB, within 60 seconds. On timeout or interrupt it is killed.
- A failure is reported in one line, without the command's output.

It does not run:

- when `ERMITE_TOKEN` is set;
- for `ermite version`, `ermite config` or shell completion;
- when `ERMITE_URL` points to another server than the file's `url` (the call is anonymous; set `ERMITE_TOKEN`);
- when the file or its directory is writable by other users, or owned by another user (not checked on Windows).

## Commands

```sh
ermite repo list | show <name> | create <name> [--description …] [--visibility public]
ermite repo edit <name> [--description …] [--visibility …] [--default-branch …] [--merge-modes …] [--allow-auto-merge] [--archived[=false]] [--issues off|owner|public]
ermite repo delete <name> [--yes]

ermite token create <label> [--preset cli|agent|ci|runner] [--scope …] [--repo …] [--branch-prefix …] [--expires 30d]
ermite token list | revoke <id>

ermite mr create <repo> --source <branch> --title … | list <repo> | show <repo> <n> [--diff]   # show prints the mergeability: up to date, behind, conflict
ermite mr comment <repo> <n> -m … [--path … --line N|A-B --side old|new] [--reply <id>] [--approve|--request-changes]
ermite mr resolve <repo> <n> <comment> | edit-comment <repo> <n> <comment> -m … | delete-comment <repo> <n> <comment>
ermite mr ready <repo> <n>                 # removes WIP:/[WIP], needs mr:write
ermite mr merge <repo> <n> --head <id> [--mode fast-forward|squash|merge] [--delete-branch] [--ignore-checks] | merge <repo> <n> --head <id> --auto [--mode …] | merge <repo> <n> --cancel-auto | close <repo> <n>

ermite issue create <repo> --title … [--body-file PATH|-] | list <repo> [--state open|closed|resolved|all]
ermite issue show <repo> <n> | comment <repo> <n> -m … | close <repo> <n> | resolve <repo> <n> | reopen <repo> <n>
# issue show prints `resolved by !N` when a merge resolved it, linked merge requests, and timestamped history interleaved with comments; --json adds closed_by_merge_request, references and events
# mr show lists linked issues (number, state, resolves marker) and their history; --json adds issues and issue_events

ermite run list <repo> [--workflow …] [--branch …] [--status …]
ermite run view <repo> <id> [--attempt n] [--log [--job …]] [--summary]
ermite run watch <repo> <id> [--log] | cancel <repo> <id> | rerun <repo> <id> [--failed]
ermite run artifacts <repo> <id> | download <repo> <id> [--name N]… [--dir D]
ermite workflow run <file> --repo <repo> [--ref …] [-f name=value …]
ermite workflow list <repo> [--json]
ermite workflow disable <repo> <file>    # no event starts a run of it; the file need not exist yet
ermite workflow enable <repo> <file>
ermite runner list | remove <id>
ermite secret set|list|delete …         # CI secrets (needs ERMITE_SECRETS_KEY)
ermite variable set|get|list|delete …
ermite environment create|list|delete … --repo <repo>

ermite mirror key show | rotate          # SSH key to add to the remote (rotate: owner only)
ermite mirror config [--enabled] [--url …{repo}…] [--cron …] [--all-branches] | config show   # global policy, owner only
ermite mirror add <repo> <url> [--cron …] [--all-branches] | remove <repo> | exclude <repo> | include <repo>
ermite mirror status [repo] | sync <repo>|--all [--force]   # sync --all: one row per repository, exit 1 if any failed

ermite deps list [--repo <repo>] [--json]   # pending dependency updates (Bernard), owner only
ermite deps check <repo>                 # check one repository now
ermite deps open|ignore|snooze <repo>    # open/update the merge request, ignore these versions, hide for 7 days

ermite journal [--since 24h|7d|2026-10-01|<RFC 3339>] [--repo R] [--kind mr_merged,tag_created,…] [--limit N] [--json]
ermite release pending [repo] [--json]   # repositories with commits after their latest tag

ermite web login                         # one-time sign-in link (60 s)
ermite web logout --all                  # sign every browser out
ermite web passkey list | remove <id>
ermite signing-key show                  # public key of squash and merge commits (server-signed)
ermite oidc rotate-key
ermite watch [repo] [--json]             # follow live events
ermite version                           # client and server versions
ermite completion bash|zsh|fish
```

### Archived repositories

`ermite repo edit <repo> --archived` makes a repository read-only; `--archived=false` unarchives it and restores writes. Archiving needs `repo:admin` and a server with the `repos.archived` feature (`ermite version`).

Archived repositories reject pushes of any ref, new or edited merge requests and issues, comments, merges (including auto-merge), release writes, workflow dispatches and reruns with “repository is archived”. Cron schedules do not fire. Reads, clones and closing existing merge requests or issues remain available. Other repository edits require unarchiving in the same request, which may include other flags.

The web shows an **Archived** badge and hides write actions. `repo show` displays the state; CLI and GitHub repository JSON include `archived`.

### Dependency updates

`ermite deps` drives Bernard, which needs a server with the `deps` feature and owner rights. `deps list` shows the pending updates, snoozed repositories included; `--json` prints `repo`, `updates` (`ecosystem`, `name`, `directory`, `current`, `latest`, `kind`, `new`), `checked_at`, `snoozed_until`, `merge_request_number`, `state`, `error` and `run_id`. The table has a `RUN` column with the run applying the updates, or the last one that tried (`-` for none); for a failed one, read its log with `ermite run view <repo> <run> --log`. `deps check <repo>` runs a check now. `deps open` and `deps ignore` read the current list and send it back as the versions you confirm, so a list that changed in between is refused with a failed precondition. When the server runs with `ERMITE_DEPS_ENABLED=0`, `deps list` is empty and the other commands answer “Bernard is disabled”. See [Bernard](bernard.md).

### Activity journal

`ermite journal` lists what happened in the repositories, newest first: merge requests and issues opened, merged or closed, tags created or deleted, releases published and pushes to default branches. Entries are kept 30 days. `--since` defaults to `24h` and takes a duration (`90m`, `7d`, `2w`), a date (midnight UTC) or an RFC 3339 time; `--kind` takes `mr_opened`, `mr_merged`, `mr_closed`, `issue_opened`, `issue_closed`, `tag_created`, `tag_deleted`, `release_published`, `push` (comma-separated or repeated); pages are followed up to `--limit` (default 100). The table shows `WHEN`, `REPO`, `KIND`, `DETAIL`, `ACTOR`. `--json` prints an array of `id`, `kind`, `repo`, `at`, `actor`, `number` (merge request or issue, 0 otherwise), `ref` (tag, pushed branch, or target branch of a merge request), `title`, `commit_count` (pushes, at most 100), `commits_capped`, `commit`, `summary`.

`ermite release pending [repo]` lists the non-archived repositories whose default branch has commits after its nearest tag, for example `ermite-api  4 commits since v0.12.0 (3 days ago)`. Repositories without a tag are left out; counts stop at 500 (`500+`). `--json` prints an array of `repo`, `latest_tag`, `latest_tag_at`, `commits_since`, `commits_capped` (true when the walk stopped at the limit before finding a tag; `commits_since` is then a lower bound), `default_branch`.

Both need a server with the `journal` feature and owner rights or the `journal:read` scope. A token limited with `--repo` sees only its repositories.

### Issues and mirrors

Issues need a server with the `issues` feature and `ermite repo edit <repo> --issues owner` (or `--issues public`, which also opens a moderated form to visitors of a public repository); they are written with the `issue:write` scope. An issue is `open`, `closed` (not resolved), or `resolved`. `issue resolve <repo> <n>` marks an open issue fixed (`issues.resolve` feature); merging a request with `Fixes #N` resolves it too. `issue reopen` accepts closed and resolved issues. History records references, closing, reopening and resolution with the actor and timestamp; removed references keep their history. See [Issues](issues.md).

Mirrors push repositories to a remote over SSH, never forcing. See [Push mirror](mirror.md).

## Releases

Releases need a server advertising the `releases` feature. Published releases follow
repository visibility; drafts are visible only to release writers. Writes need
`release:write` (included in the `cli` token preset); branch-limited tokens cannot
write releases. On an older server the preset drops this scope with a note, while
an explicit `--scope release:write` fails before creating a token.

```sh
ermite release create <repo> <tag> [--title T] [--notes TEXT | --notes-file F] [--target REF] [--draft] [--prerelease] [--latest] [--asset FILE[#LABEL]]… [--json]
ermite release list <repo> [--limit N] [--json]
ermite release view <repo> <tag|id|latest> [--json | --notes]
ermite release edit <repo> <tag|id> [--title T] [--notes TEXT | --notes-file F] [--tag NEW] [--target REF] [--draft | --publish] [--prerelease | --no-prerelease] [--latest]
ermite release delete <repo> <tag|id> [--yes]
ermite release upload <repo> <tag|id> FILE[#LABEL]… [--clobber] [--content-type T]
ermite release download <repo> <tag|id|latest> [--pattern GLOB]… [--name N]… [--dir D]
```

All-digit selectors are release ids; other selectors are tags. `latest` selects
the latest release for view/download. `--latest` pins a release as latest.
Non-draft releases require an existing tag, or `--target` to create one; an
existing tag is never moved. `--notes-file -` reads Markdown from standard input.
Edit changes only supplied fields: `--title=''` and `--notes=''` clear them;
`--publish` and `--no-prerelease` clear the corresponding flags.

Create uploads all `--asset` files, then prints the release web URL and id.
Each asset's name is the file basename; an optional `#LABEL` sets its display
label (quote it). Upload accepts multiple file arguments, including shell-expanded
globs. For create, repeat `--asset` for each file. Content type is detected from
the extension, or sniffed from the first bytes; upload's `--content-type` overrides
it. A duplicate name fails with a hint to use `--clobber`. A failed upload after
create leaves the release in place and reports its id.

List defaults to 30 entries and follows server pagination up to `--limit`.
View shows fields, notes and an asset table; `--notes` prints Markdown alone.
Create, list and view support stable JSON (fields are listed in `ermite release --help`).
Delete removes the release and assets, not its Git tag; it asks for confirmation
only on a terminal unless `--yes` is given.

Download defaults to all assets, into `--dir` (default `.`). Repeat `--name` or
quote and repeat `--pattern` to select a union of names and globs. Files are never
overwritten. One interrupted stream is resumed automatically from the bytes
already received; size and SHA-256 digest are verified. Failed or mismatching
downloads are removed. Resume is within the command, not across invocations.

## Output

- `--json` prints stable field names (listed in each `--help`) on `repo list|show|create|edit`, `issue create|list|show|comment|close|resolve|reopen`, `token create|list`, `run list|view|cancel|rerun`, `runner list`, `mirror key|config|status`, `deps`, `journal`, `release pending` and `version`.
- `watch --json` prints one object per line. `merge_request_mergeability` events carry `mergeability` (`state`, `behind`, `conflict_paths`…) and print as `repo!n mergeability: behind by 2`.
- A merge needs passing checks on the head (no failed, queued or running checks); a head with no checks can merge. `mr merge --ignore-checks` bypasses this, needs `mr:force-merge`, and cannot be combined with `--auto` or `--cancel-auto`. The `cli` preset includes `mr:force-merge`; `agent` does not. Adding `--scope mr:merge` lets an agent merge only with passing checks.
- `mr merge --mode squash|merge` creates a commit signed by the server's own key (generated on first start); the output names its fingerprint, `ermite signing-key show` prints the key. Every mode is allowed by default; `repo edit --merge-modes fast-forward,squash` disables `merge`.
- `mr merge --auto` makes the server merge once CI passed on the head (`repo edit --allow-auto-merge` first); `--cancel-auto` cancels it; both are exclusive with each other and `--auto` with `--delete-branch`. `watch --json` adds `merge_request_auto_merge_enabled`, `merge_request_auto_merge_cancelled` (with `reason`), and `auto` (true) on `merge_request_merged` after an auto-merge. See [Merge requests](merge-requests.md#auto-merge).
- Errors are one line on stderr (`ermite: …`) with exit status 1.
- Shell completion completes repository names from the server.

`ermite mr create` and `ermite mr close` print a confirmation followed by the full
merge request web URL on its own line. With `--json`, merge request objects
(create, show, list, close and merge) include `url` alongside the other fields.

`ermite issue create` prints a confirmation followed by the full issue web URL on its own line
(`issue close` and `issue reopen` do the same). With `--json`, issue objects include `url`.

### Edit issue and merge request text

```sh
ermite issue edit <repo> <number> [--title T] [--body-file PATH|-] [--json]
ermite mr edit <repo> <number> [--title T] [--description-file PATH|-] [--json]
```

Supply at least one edit flag. Bodies and descriptions come only from files; `-` reads stdin, with a 1 MiB limit. An empty file clears the text. Omitted fields stay unchanged. Commands print “Edited” and the item URL, or the updated object with `--json`. The scopes are `issue:write` and `mr:write`; servers advertise `issues.edit` and `merge_requests.edit`.

Text edits publish dedicated `issue_edited` and `merge_request_edited` live events, not comment events. `ermite watch` shows the repository, item number and `changed_fields` (`title`/`body` or `title`/`description`); unknown event kinds are ignored.

Issue and merge request creation accept body/description text only from files: `issue create <repo> --title T [--body-file PATH|-]` and `mr create <repo> --source B --title T [--description-file PATH|-]`. Both file flags are optional; omitting them creates empty text. `-` reads stdin, with a 1 MiB limit shared with edit commands. Inline `--body` and `--description` are not accepted for these commands. Repository descriptions remain inline.
