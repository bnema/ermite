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
3. Comment on a line: `ermite mr comment <repo> <n> -m "text" --path <file> --line <n> --side new`. Without `--path` the comment is general. The line must appear in the diff.
4. Merge only when asked: `ermite mr merge <repo> <n> --head <head_id>`, with `--squash` for one commit signed by the server and `--delete-branch` to delete the source branch afterwards.

The merge is refused when the branch moved since `head_id` (review again) or when the target is not an ancestor of the head (rebase and push again). The repository may allow only one merge mode.

`ermite mr create` and `ermite mr close` print a confirmation followed by the full
merge request web URL on its own line. With `--json`, merge request objects
(create, show, list, close and merge) include `url` alongside the other fields.

Open one: `ermite mr create <repo> --source <branch> --title "..."`. After new pushes, `mr show --since-last-read` shows only what changed since `--mark-read`.

## Tokens

```sh
ermite token create my-agent --preset agent --repo <repo> --expires 30d
```

The secret is printed once. Presets: `agent` (push, open and comment on merge requests), `ci` (read), `cli` (everything). Add `--scope mr:merge` to let an agent merge. A token can never grant more than the token that creates it. Full scope list: [CLI.md](CLI.md).

## Follow events

`ermite watch [repo] --json` prints one JSON object per event (pushes, repositories, merge requests, comments). It reconnects and resumes on its own; a `gap` event means some events were lost, so reload the state.

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

## Output conventions

`--json` prints stable field names, listed in each command's `--help`. Errors are one line on standard error (`ermite: ...`) with exit status 1. `ermite completion bash|zsh|fish` completes repository names.

Every command, flag, scope and JSON field: [CLI.md](CLI.md).
