---
name: ermite
description: Explains how to use an ermite Git forge and its CLI - clone and push over HTTPS, repositories, tokens and scopes, merge requests (review, comment, merge), live events, and running the server. Use when the user mentions ermite, runs `ermite` commands, pushes to or reviews on an ermite server, or self-hosts one.
---

# ermite

ermite is a single-user Git forge in one binary. `ermite serve` runs the server; every other `ermite` command is a client that talks to a running server over its API. The owner holds the admin token and creates scoped tokens for themselves, agents and CI.

## Connect

```sh
export ERMITE_URL=https://git.example.com
export ERMITE_TOKEN=ermite_...        # optional for public reads
ermite version                        # client and server versions, warns about missing features
ermite repo list
```

Instead of exporting them, the owner can keep `url` and a `token.cmd` (a command that prints the token, such as `pass show ermite/token`) in `~/.config/ermite/config`; as an agent, set `ERMITE_TOKEN` or point `ERMITE_CONFIG` at your own file.

Never print a token, commit it, log it or paste it into a chat. Prefer a Git credential helper to a token in a URL. Use `https://`: a token sent over plain `http://` to a remote host triggers a warning.

## Git

```sh
git clone https://git.example.com/<repo>.git                 # public: no credentials
git push https://git.example.com/<repo>.git main             # password = token, username ignored
```

- Pushing to a name that does not exist creates a private repository (needs `repo:write`).
- Names are lowercase `[a-z0-9._-]`, 1 to 100 characters.
- Private repositories need `repo:read` to clone or browse.

## Review a merge request

1. List: `ermite mr list <repo> --json`.
2. Read: `ermite mr show <repo> <n> --diff --json`. Keep `head_id`; take line numbers from the hunks.
3. Comment on a line: `ermite mr comment <repo> <n> -m "text" --path <file> --line <n> --side new`; `--line 10-14` covers a range on one side. Without `--path` the comment is general. The line must appear in the diff. Notes are Markdown; `^N` refers to note N.
4. End a review with a verdict (needs `mr:review`): `--approve` or `--request-changes`, alone or with a message. Verdicts are informative and cannot be edited; a new verdict replaces the previous one.
5. Merge only when the human asks explicitly. If the request is a draft, run `ermite mr ready <repo> <n>` first (removes the `WIP:` or `[WIP]` prefix; needs `mr:write`). Merge: `ermite mr merge <repo> <n> --head <head_id>`, with `--mode squash` for one commit or `--mode merge` for a merge commit (both signed by the server's key, see `ermite signing-key show`) and `--delete-branch` to delete the source branch afterwards.

To merge once CI passed automatically, add `--auto` (`ermite mr merge <repo> <n> --head <head_id> --auto --mode squash`; needs `repo edit --allow-auto-merge`); `--cancel-auto` cancels it.

A merge needs passing checks on the head (none failed, queued or running); a head with no checks can merge. Never use `--ignore-checks` unless the human asks for it. It needs `mr:force-merge` and cannot be combined with `--auto` or `--cancel-auto`.

The merge is refused when the branch moved since `head_id` (review again) or, for `fast-forward`, when the target is not an ancestor of the head (rebase and push again, or use `squash`/`merge`), or when `squash`/`merge` conflict. `ermite mr show` prints the `mergeability` line (up to date, behind by N, conflict with paths). The repository may allow only some merge modes.

### Act on a review

Notes are visible as soon as they are written, so a review is not finished until its verdict. Act on a verdict, not on each note (`ermite watch --json` shows `verdict` on the `comment_added` event).

On `changes_requested`: run `ermite mr show <repo> <n> --json`, read the open threads (roots with `resolved` false and their replies), fix the code, push, then answer each thread with `ermite mr comment <repo> <n> --reply <id> -m "Fixed in ^N"`. Leave resolving to the reviewer: do not run `mr resolve` on their threads. Edit or delete only your own notes (`mr edit-comment`, `mr delete-comment`).

An approval is stale after a push; changes requested stay until the reviewer gives a new verdict.

`ermite mr create` and `ermite mr close` print a confirmation followed by the full
merge request web URL on its own line. With `--json`, merge request objects
(create, show, list, close and merge) include `url` alongside the other fields.

Open one: `ermite mr create <repo> --source <branch> --title "..."`. After new pushes, `mr show --since-last-read` shows only what changed since `--mark-read`.

## Issues

Enable issues with `ermite repo edit <repo> --issues owner` (or `public`).
Use `issue create <repo> --title "..."`, `issue list <repo> --state open|closed|resolved|all`,
and `issue show <repo> <n> --json`. `issue close` means closed without resolution;
`issue resolve <repo> <n>` marks an open issue fixed (needs `issues.resolve`), and
`issue reopen <repo> <n>` reopens a closed or resolved issue. Writes need `issue:write`.
Merging an MR with `Fixes #N` resolves the open issue. Issue detail shows `resolved by !N`
and timestamped history with actors alongside comments; JSON includes `events` and `references`.
`mr show` lists linked issues, their states and the resolves marker, with history rows;
JSON includes `issues` and `issue_events`. Removed references retain their history.

## Tokens

```sh
ermite token create my-agent --preset agent --repo <repo> --expires 30d
```

The secret is printed once. Presets: `agent` (push, open and comment on merge requests; no verdicts), `ci` (read), `cli` (every scope, including `mr:force-merge`). Add `--scope mr:review` to let an agent give verdicts, `--scope mr:merge` to let it merge only with passing checks. The `agent` preset does not include `mr:force-merge`. A token can never grant more than the token that creates it. Full scope list: [CLI.md](CLI.md).

## Follow events

`ermite watch [repo] --json` prints one JSON object per event (pushes, repositories, merge requests, notes: added, edited, deleted, resolved). It reconnects and resumes on its own; a `gap` event means some events were lost, so reload the state.

## Run a server

```sh
token=$(head -c 32 /dev/urandom | base64)   # the root credential: store it safely
docker run -d -p 127.0.0.1:7341:7341 -v ermite-data:/data \
  -e ERMITE_PUBLIC_URL=https://git.example.com -e ERMITE_ADMIN_TOKEN="$token" <ermite-image>
```

- Required: `ERMITE_DATA_DIR` (the image uses `/data`), `ERMITE_PUBLIC_URL`, `ERMITE_ADMIN_TOKEN` (32+ characters). It needs git 2.45 or newer.
- Put TLS on a reverse proxy and list its network in `ERMITE_TRUSTED_PROXIES`.
- Health check: `GET /-/health`.
- Owner web access: `ermite web login` prints a one-time link (about 60 s, 12 h session).
- If a page or a read looks wrong, set `ERMITE_GIT_READER=cli` and restart: every read then uses the `git` binary.

## Releases from CI

In a CI job, `GITHUB_API_URL` and `GITHUB_SERVER_URL` point at an address the job can reach (not necessarily the public URL). `softprops/action-gh-release` uses them as is. GoReleaser and other tools with hard-coded GitHub URLs must take them from these variables, never the public URL alone (the job times out):

```yaml
github_urls:
  api: '{{ or (index .Env "GITHUB_API_URL") "https://git.example.org/-/github/api" }}/'
  upload: '{{ or (index .Env "GITHUB_API_URL") "https://git.example.org/-/github/api" }}/uploads/'
  download: '{{ or (index .Env "GITHUB_SERVER_URL") "https://git.example.org" }}/'
```

The workflow needs `permissions: contents: write` and `GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}`. Enable an existing one with `ermite workflow enable <repo> .github/workflows/release.yml`. Full example: `docs/releases.md` in the ermite repository.

## Output conventions

`--json` prints stable field names, listed in each command's `--help`. Errors are one line on standard error (`ermite: ...`) with exit status 1. `ermite completion bash|zsh|fish` completes repository names.

Every command, flag, scope and JSON field: [CLI.md](CLI.md).

### Edit existing text

Use `ermite issue edit <repo> <number> --title "..." --body-file issue.md` or `ermite mr edit <repo> <number> --title "..." --description-file mr.md`. Supply either or both flags; body/description text comes only from files, with `-` for stdin and an empty file to clear it. Both support `--json`. Issues need `issue:write`; merge requests need `mr:write`. Pending issues and merged requests cannot be edited.

Text edits publish dedicated `issue_edited` and `merge_request_edited` live events, not comment events. `ermite watch` shows the repository, item number and `changed_fields` (`title`/`body` or `title`/`description`); unknown event kinds are ignored.

Issue and merge request creation accept body/description text only from files: `issue create <repo> --title T [--body-file PATH|-]` and `mr create <repo> --source B --title T [--description-file PATH|-]`. Both file flags are optional; omitting them creates empty text. `-` reads stdin, with a 1 MiB limit shared with edit commands. Inline `--body` and `--description` are not accepted for these commands. Repository descriptions remain inline.
