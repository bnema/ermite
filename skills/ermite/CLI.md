# ermite CLI reference

Remote commands read `ERMITE_URL` (`http://` or `https://`) and `ERMITE_TOKEN` (optional for public reads); they fall back to the `url` and `token.cmd` of the config file (`~/.config/ermite/config`, the platform config dir on macOS and Windows, or the file named by `ERMITE_CONFIG`; `ermite config show` prints the path), the environment first (`token.cmd` is only run for the server of the file: with a different `ERMITE_URL` set `ERMITE_TOKEN` too). Agents: use `ERMITE_TOKEN`, or your own `ERMITE_CONFIG` file; do not edit the owner's `~/.config/ermite/config` and never run `token.cmd` yourself or print what it prints. Run `ermite <command> --help` for the authoritative text of a command and its JSON fields.

Errors are one line on standard error (`ermite: ...`) with exit status 1. `--json` prints stable field names. `ermite completion bash|zsh|fish` completes repository names from the server.

## Repositories

| Command | Scope | Notes |
|---|---|---|
| `repo list [--json]` | none | repositories you can see |
| `repo show <name> [--json]` | none | |
| `repo create <name> [--description TEXT] [--visibility public]` | `repo:write` | private by default |
| `repo edit <name> [--description TEXT] [--visibility V] [--default-branch B] [--merge-modes fast-forward,squash,merge] [--failing-checks-block=false]` | `repo:admin` | only the given flags change; every merge mode is allowed by default, list the ones to keep; `--description ""` clears it; the branch must exist |
| `repo delete <name> [--yes]` | `repo:admin` | |

Repository JSON: `name`, `description`, `visibility`, `default_branch`, `created_at`, `pushed_at` (null before the first push), `merge_modes`, `failing_checks_block`.

## Tokens

| Command | Scope | Notes |
|---|---|---|
| `token create <label> [--preset cli\|agent\|ci] [--scope S] [--repo R] [--branch-prefix P] [--expires 30d\|2w\|12h\|2027-01-31] [--json]` | `token:admin` | secret printed once on standard output; there is no default scope |
| `token list [--json]` | `token:admin` | secrets are never shown; the admin token is not listed |
| `token revoke <id>` | `token:admin` | |

Presets add up with `--scope` (repeatable or comma-separated):

| Preset | Scopes |
|---|---|
| `cli` | every scope (including `mr:review`), for the owner's daily use; a token created before `mr:review` existed lacks it and loses owner rights: recreate it with `--preset cli` using the root token or from a signed-in owner web session (an old token lacking `mr:review` cannot grant it), then replace the stored CLI secret and revoke the old token |
| `agent` | `repo:write`, `mr:write` |
| `ci` | `repo:read` |

Scopes:

| Scope | Allows |
|---|---|
| `repo:read` | read private repositories |
| `repo:write` | push, create a repository |
| `repo:admin` | delete a repository, change visibility, description, default branch, merge settings |
| `mr:write` | open, comment on, reply, edit and delete own notes, resolve, close, mark read |
| `mr:review` | give a verdict (`--approve`, `--request-changes`) |
| `mr:merge` | merge, and delete the merged source branch with `--delete-branch` |
| `issue:write` | open, comment on, close and reopen issues |
| `token:admin` | create, list and revoke tokens |
| `ci:write`, `secrets:write`, `runner:register` | reserved for CI |

A token can never grant more than the token that creates it. `--repo` (repeatable) limits it to those repositories; the default is all. Labels are free text up to 100 bytes; `owner` and `root` are reserved. Token JSON: `id`, `label`, `scopes`, `repos`, `branch_prefixes`, `expires_at`, `created_at`, `last_used_at`.

`--branch-prefix` (repeatable, up to 8, each ending with `/`, for example `agent/claude/`) limits the branches the token may write; the default is all. A limited token pushes, force-pushes and deletes only `refs/heads/<prefix>…`: any other ref (`main`, tags, notes) refuses the whole push, naming the ref. It cannot create or delete a repository or change its settings (`repo edit`: default branch, visibility, description, merge modes), merges only into branches under its prefixes (and deletes only a source branch under them), never has owner rights, and creates only tokens within its own prefixes. A token for an agent: `ermite token create claude --preset agent --scope repo:read --repo <repo> --branch-prefix agent/claude/`. If you hold such a token, work on a branch under your prefix and open a merge request; do not try `main` or tags.

## Merge requests

| Command | Scope | Notes |
|---|---|---|
| `mr create <repo> --source B --title T [--target B] [--description TEXT\|-]` | `mr:write` | target defaults to the default branch; title of 120 characters max; `-` reads standard input |
| `mr list <repo> [--state open\|merged\|closed\|all] [--json]` | none | default `open` |
| `mr show <repo> <n> [--diff] [--since-last-read] [--mark-read] [--json]` | `mr:write` for `--mark-read` | commits, changed files and comments; `--diff` adds hunks with line numbers; the `mergeability` line (JSON `mergeability`, null unless open) is `up_to_date`, `behind` (N commits, merges cleanly), `conflict` (paths) or `checking` |
| `mr comment <repo> <n> -m TEXT\|- [--path P --line N\|A-B --side old\|new] [--reply ID] [--approve\|--request-changes]` | `mr:write`; verdicts `mr:review` | Markdown note (8000 characters max); the lines must be in the current diff on one side; a deleted file uses its old path; `--reply` takes a root note (`12`, `#12` or `^12`), inherits its anchor and cannot carry a verdict; a verdict note may have no message |
| `mr edit-comment <repo> <n> <comment-id> -m TEXT\|-` | `mr:write` | author only, open merge request; the verdict is unchanged |
| `mr delete-comment <repo> <n> <comment-id>` | `mr:write` | author only; the text is lost, the verdict is kept |
| `mr resolve <repo> <n> <comment-id> [--undo]` | `mr:write` | on the thread root, covers its replies; doing it twice is not an error |
| `mr merge <repo> <n> --head <full-id> [--mode fast-forward\|squash\|merge] [--delete-branch]` | `mr:merge` | refused if the branch moved, if `fast-forward` and the target is not an ancestor of the head (behind: use `squash` or `merge`), or if `squash`/`merge` conflict; squash and merge commits are signed by the server key (`signing-key show`); answers once the target moved; `--delete-branch` deletes the source after the merge (refused for the default branch or a branch another open request uses; JSON adds `source_branch_deleted`) |
| `mr close <repo> <n>` | `mr:write` | a closed merge request cannot be reopened |

`ermite mr create` and `ermite mr close` print a confirmation followed by the full
merge request web URL on its own line. With `--json`, merge request objects
(create, show, list, close and merge) include `url` alongside the other fields.

Merge request JSON: `repo`, `url` (full web URL), `number`, `title`, `description`, `source_branch`, `target_branch`, `state`, `author` (token label), `created_at`, `updated_at`, `head_id`, `merged_commit_id`, `merge_mode`. `mr show --json` adds `commits`, `diff_base_id`, `stats`, `files` (with `hunks` under `--diff`: `kind`, `old_number`, `new_number`, `text`), `comments`, `verdicts`, `last_read_id`, `last_read_lost`.

Comment JSON: `id`, `author`, `path`, `line`, `side`, `commit_id`, `text`, `resolved`, `created_at`, `reply_to` (empty for roots), `start_line` (0 for a single line), `verdict` (`""`, `approve`, `changes_requested`), `edited_at`, `deleted`. Verdict JSON (latest per author): `author`, `verdict`, `comment_id`, `commit_id`, `created_at`, `stale` (an approval given on another head; changes requested are never stale). Verdicts are informative and never block a merge. In notes, `^N` refers to note N.

## Issues

Issues share the numbering of merge requests and must be enabled per repository: `repo edit <repo> --issues off|owner|public` (`repo:admin`; default `off`; `public` also opens a moderated form to visitors of a public repository). Reading and writing needs a server with the `issues` feature.

| Command | Scope | Notes |
|---|---|---|
| `issue create <repo> --title T [--body TEXT\|-] [--body-file F\|-]` | `issue:write` | title of 120 characters max |
| `issue list <repo> [--state open\|closed\|all] [--json]` | read access (public repository, or `repo:read` on a private one) | default `open`; `issue:write` alone does not grant reading |
| `issue show <repo> <n> [--json]` | read access (public repository, or `repo:read` on a private one) | issue and comments; `issue:write` alone does not grant reading |
| `issue comment <repo> <n> -m TEXT\|-` | `issue:write` | |
| `issue close <repo> <n>` / `issue reopen <repo> <n>` | `issue:write` | |

Issue JSON: `repo`, `number`, `title`, `body`, `state`, `author` (token label), `created_at`, `updated_at`, `comment_count`. `issue show --json` adds `comments` (`id`, `author`, `text`, `created_at`). `repo show --json` has `issues`.

## Workflows

| Command | Scope | Notes |
|---|---|---|
| `workflow list <repo> [--json]` | `repo:read` for a private repository | the files of `.github/workflows` on the default branch with their state, and the disabled files that branch lacks |
| `workflow disable <repo> <file> [--json]` | `repo:admin` | like `gh workflow disable`: no event (push, pull_request, pull_request_target, schedule, workflow_run, create, delete, workflow_dispatch) starts a run of it; runs already started go on; `<file>` is `release.yml` or `.github/workflows/release.yml` and need not exist, so it works on an empty repository before the first push; doing it twice is not an error |
| `workflow enable <repo> <file> [--json]` | `repo:admin` | undoes `disable`; missed events are not replayed |
| `workflow run <file> --repo R [--ref REF] [-f name=value]` | `ci:write` | starts a `workflow_dispatch` workflow; refused while the workflow is disabled |

Workflow JSON: `repo`, `file` (always `.github/workflows/<file>`), `name` (the `name:` on the default branch, empty when the file is absent there), `state` (`enabled` or `disabled`). A token limited to branch prefixes cannot disable or enable.

## Events

`ermite watch [repo] [--json]` runs until interrupted, reconnects and resumes after the last event. With `--json` it prints one object per line with `id`, `time`, `type` and type-specific fields.

| Type | Extra fields |
|---|---|
| `repo_created`, `repo_updated`, `repo_deleted` | `repo`, `repository` |
| `pushed` | `repo`, `ref_updates` (`ref`, `old_id`, `new_id`; an empty id means created or deleted) |
| `merge_request_opened`, `merge_request_merged`, `merge_request_closed` | `repo`, `merge_request` |
| `merge_request_mergeability` | `repo`, `number`, `mergeability` (`state`: `checking`, `up_to_date`, `behind`, `conflict`; `behind`, `behind_capped`, `conflict_paths`, `conflict_paths_truncated`); sent when the state changes |
| `comment_added`, `comment_edited`, `comment_deleted`, `comment_resolved` | `repo`, `number`, `comment` (a verdict note has `verdict` set) |
| `gap` | none: events were lost, reload the state |

## Server and web

| Command | Notes |
|---|---|
| `serve` | configured by `ERMITE_*` environment variables only (see the project README) |
| `version [--json]` | client version; with a server URL (`ERMITE_URL` or the config file), the server version and features |
| `config show [--json]` / `config set <url\|token.cmd> <value>` / `config unset <key>` | the CLI config file; `token.cmd` is a command that prints the token (any secret manager), never the token itself; `show` never runs it |
| `signing-key show` | public key and `allowed_signers` line to check squash and merge commits with `git verify-commit` |
| `signing-key trust list [--json]` | keys commit signatures are checked against (the server key first) |
| `signing-key trust add --format ssh\|gpg --title <t> --email <e>... <file\|->` / `trust remove <id>` | owner only; a commit shows verified when its committer email is listed for the key |
| `web login [--json]` | one-time sign-in link (about 60 s); needs the admin token or a token with every scope, no repository list and no branch prefix; the link is a secret |
| `web logout --all` | signs every browser out |
| `web passkey list [--json]` | the owner's passkeys (`id`, `name`, `created_at`, `last_used_at`, `synced`); same rights as `web login`; fails with "passkeys are not available on this server" when the public URL is not https with a host name |
| `web passkey remove <id>` | removes a passkey and signs every browser out; the id comes from `list` |

Adding a passkey is a browser action (`/-/passkeys` once signed in); agents do not use passkeys.
