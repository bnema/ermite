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
ermite repo edit <name> [--description …] [--visibility …] [--default-branch …] [--merge-modes …] [--failing-checks-block=false]
ermite repo delete <name> [--yes]

ermite token create <label> [--preset cli|agent|ci|runner] [--scope …] [--repo …] [--branch-prefix …] [--expires 30d]
ermite token list | revoke <id>

ermite mr create <repo> --source <branch> --title … | list <repo> | show <repo> <n> [--diff]
ermite mr comment <repo> <n> -m … [--path … --line … --side old|new] | resolve <repo> <n> <comment>
ermite mr merge <repo> <n> --head <id> [--squash] [--delete-branch] | close <repo> <n>

ermite run list <repo> [--workflow …] [--branch …] [--status …]
ermite run view <repo> <id> [--attempt n] [--log [--job …]] [--summary]
ermite run watch <repo> <id> [--log] | cancel <repo> <id> | rerun <repo> <id> [--failed]
ermite run artifacts <repo> <id> | download <repo> <id> [--name N]… [--dir D]
ermite workflow run <file> --repo <repo> [--ref …] [-f name=value …]
ermite runner list | remove <id>
ermite secret set|list|delete …         # CI secrets (needs ERMITE_SECRETS_KEY)
ermite variable set|get|list|delete …
ermite environment create|list|delete … --repo <repo>

ermite web login                         # one-time sign-in link (60 s)
ermite web logout --all                  # sign every browser out
ermite web passkey list | remove <id>
ermite signing-key show                  # public key of squash commits
ermite oidc rotate-key
ermite watch [repo] [--json]             # follow live events
ermite version                           # client and server versions
ermite completion bash|zsh|fish
```

## Output

- `--json` prints stable field names (listed in each `--help`) on `repo list|show|create|edit`, `token create|list`, `run list|view|cancel|rerun`, `runner list` and `version`.
- `watch --json` prints one object per line.
- Errors are one line on stderr (`ermite: …`) with exit status 1.
- Shell completion completes repository names from the server.
