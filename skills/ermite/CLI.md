# ermite CLI reference

Remote commands read `ERMITE_URL` (`http://` or `https://`) and `ERMITE_TOKEN` (optional for public reads); they fall back to the `url` and `token.cmd` of the config file (`~/.config/ermite/config`, or the file named by `ERMITE_CONFIG`), the environment first. Agents: use `ERMITE_TOKEN`, or your own `ERMITE_CONFIG` file; do not edit the owner's `~/.config/ermite/config` and never run `token.cmd` yourself or print what it prints. Run `ermite <command> --help` for the authoritative text of a command and its JSON fields.

Errors are one line on standard error (`ermite: ...`) with exit status 1. `--json` prints stable field names. `ermite completion bash|zsh|fish` completes repository names from the server.

## Repositories

| Command | Scope | Notes |
|---|---|---|
| `repo list [--json]` | none | repositories you can see |
| `repo show <name> [--json]` | none | |
| `repo create <name> [--description TEXT] [--visibility public]` | `repo:write` | private by default |
| `repo edit <name> [--description TEXT] [--visibility V] [--default-branch B] [--merge-modes fast-forward,squash] [--failing-checks-block=false]` | `repo:admin` | only the given flags change; `--description ""` clears it; the branch must exist |
| `repo delete <name> [--yes]` | `repo:admin` | |

Repository JSON: `name`, `description`, `visibility`, `default_branch`, `created_at`, `pushed_at` (null before the first push), `merge_modes`, `failing_checks_block`.

## Tokens

| Command | Scope | Notes |
|---|---|---|
| `token create <label> [--preset cli\|agent\|ci] [--scope S] [--repo R] [--expires 30d\|2w\|12h\|2027-01-31] [--json]` | `token:admin` | secret printed once on standard output; there is no default scope |
| `token list [--json]` | `token:admin` | secrets are never shown; the admin token is not listed |
| `token revoke <id>` | `token:admin` | |

Presets add up with `--scope` (repeatable or comma-separated):

| Preset | Scopes |
|---|---|
| `cli` | every scope, for the owner's daily use |
| `agent` | `repo:write`, `mr:write` |
| `ci` | `repo:read` |

Scopes:

| Scope | Allows |
|---|---|
| `repo:read` | read private repositories |
| `repo:write` | push, create a repository |
| `repo:admin` | delete a repository, change visibility, description, default branch, merge settings |
| `mr:write` | open, comment on, resolve, close, mark read |
| `mr:merge` | merge, and delete the merged source branch with `--delete-branch` |
| `token:admin` | create, list and revoke tokens |
| `ci:write`, `secrets:write`, `runner:register` | reserved for CI |

A token can never grant more than the token that creates it. `--repo` (repeatable) limits it to those repositories; the default is all. Labels are free text up to 100 bytes; `owner` and `root` are reserved. Token JSON: `id`, `label`, `scopes`, `repos`, `expires_at`, `created_at`, `last_used_at`.

## Merge requests

| Command | Scope | Notes |
|---|---|---|
| `mr create <repo> --source B --title T [--target B] [--description TEXT\|-]` | `mr:write` | target defaults to the default branch; title of 120 characters max; `-` reads standard input |
| `mr list <repo> [--state open\|merged\|closed\|all] [--json]` | none | default `open` |
| `mr show <repo> <n> [--diff] [--since-last-read] [--mark-read] [--json]` | `mr:write` for `--mark-read` | commits, changed files and comments; `--diff` adds hunks with line numbers |
| `mr comment <repo> <n> -m TEXT\|- [--path P --line N --side old\|new]` | `mr:write` | the line must be in the current diff; a deleted file uses its old path |
| `mr resolve <repo> <n> <comment-id> [--undo]` | `mr:write` | doing it twice is not an error |
| `mr merge <repo> <n> --head <full-id> [--squash] [--delete-branch]` | `mr:merge` | refused if the branch moved or the target is not an ancestor of the head; `--delete-branch` deletes the source after the merge (refused for the default branch or a branch another open request uses; JSON adds `source_branch_deleted`) |
| `mr close <repo> <n>` | `mr:write` | a closed merge request cannot be reopened |

Merge request JSON: `repo`, `number`, `title`, `description`, `source_branch`, `target_branch`, `state`, `author` (token label), `created_at`, `updated_at`, `head_id`, `merged_commit_id`, `merge_mode`. `mr show --json` adds `commits`, `diff_base_id`, `stats`, `files` (with `hunks` under `--diff`: `kind`, `old_number`, `new_number`, `text`), `comments`, `last_read_id`, `last_read_lost`.

## Events

`ermite watch [repo] [--json]` runs until interrupted, reconnects and resumes after the last event. With `--json` it prints one object per line with `id`, `time`, `type` and type-specific fields.

| Type | Extra fields |
|---|---|
| `repo_created`, `repo_updated`, `repo_deleted` | `repo`, `repository` |
| `pushed` | `repo`, `ref_updates` (`ref`, `old_id`, `new_id`; an empty id means created or deleted) |
| `merge_request_opened`, `merge_request_merged`, `merge_request_closed` | `repo`, `merge_request` |
| `comment_added` | `repo`, `number`, `comment` |
| `gap` | none: events were lost, reload the state |

## Server and web

| Command | Notes |
|---|---|
| `serve` | configured by `ERMITE_*` environment variables only (see the project README) |
| `version [--json]` | client version; with a server URL (`ERMITE_URL` or the config file), the server version and features |
| `config show` / `config set <url\|token.cmd> <value>` / `config unset <key>` | the CLI config file; `token.cmd` is a command that prints the token (any secret manager), never the token itself; `show` never runs it |
| `signing-key show` | public key and `allowed_signers` line to check squash commits with `git verify-commit` |
| `web login [--json]` | one-time sign-in link (about 60 s); needs the admin token or a token with every scope and no repository list; the link is a secret |
| `web logout --all` | signs every browser out |
