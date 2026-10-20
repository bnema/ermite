# Push mirror

Ermite can push its repositories to a remote Git host over SSH (GitHub, another forge…), to keep a read-only copy there. The push never forces and never deletes anything on the remote.

## Set up

1. The server needs `ERMITE_SECRETS_KEY`; it seals the mirror SSH key. Without it, syncs fail and nothing is stored.
2. Add the server's public mirror key to the remote account or repository, as a key allowed to push:

   ```sh
   ermite mirror key show | gh ssh-key add - --title "ermite mirror"
   ```

   `mirror key show` prints only the `authorized_keys` line. One key serves every mirror.
3. Set the global policy (owner only). The URL template holds `{repo}`, replaced by the repository name; the cron is five fields, in UTC:

   ```sh
   ermite mirror config --url 'git@github.com:acme/{repo}.git' --cron '*/15 * * * *' --enabled
   ermite mirror config show
   ```

   `config` changes only the flags you pass. `--all-branches` mirrors every branch instead of the default branch.
4. Adjust single repositories:

   ```sh
   ermite mirror add <repo> <url> [--cron …] [--all-branches]   # own URL (and schedule), even when the global policy is off
   ermite mirror exclude <repo>                                  # leave the global policy
   ermite mirror include <repo>                                  # back under it
   ermite mirror remove <repo>                                   # drop the override
   ```

   Without `--cron` or `--all-branches`, `add` inherits the global values.

URLs are `git@host:path` or `ssh://[user@]host[:port]/path`, with no password. The cron may not fire more often than every 5 minutes.

## What is pushed

- The default branch and all tags; every branch with `--all-branches`.
- Never a force push, never a deletion: a branch or tag deleted here stays on the remote.
- If the remote has commits ermite lacks (diverged), the mirror is **paused**. To fix it, fast-forward ermite with the remote's commits (push them here), then run `ermite mirror sync <repo> --force`.

## When it syncs

- On the cron schedule. A push to ermite only marks the repository as changed; the next due cron tick syncs it. A tick with nothing new does nothing.
- `ermite mirror sync <repo>` or `ermite mirror sync --all` (owner only) syncs now; a repository needs admin rights.
- `--force` skips the no-op check and the 60 s throttle and lifts a pause. It never force-pushes.
- Overlapping syncs of a repository coalesce; only one push runs at a time per repository.

## Limits against spam

| Rule | Value |
|---|---|
| No-op | nothing changed since the last successful push: no connection |
| Manual sync | one per repository per 60 s, unless `--force` |
| Cron | at least 5 minutes between firings |
| Backoff after a transient failure | 5 min, doubling, up to 6 h |
| Paused | after 5 consecutive transient failures |
| Paused at once | diverged remote, key refused, remote not found, host key changed |

A paused mirror is not retried by the schedule. Changing the mirror configuration of a repository clears its failures.

## See what happens

- `ermite mirror status [<repo>] [--json]`: state (`ok`, `failing`, `paused`, `disabled`), reason, last success, next run, failures.
- A `git push` to a repository whose mirror is failing or paused prints a `remote:` warning, to people with admin rights.
- The repository page shows a badge to owners and admins.
- `ermite watch` receives `mirror.failed` once when a mirror starts failing, and `mirror.recovered` when it succeeds again. See [events](ci-events.md#mirror-events).

## Security

- The private key is sealed at rest with `ERMITE_SECRETS_KEY`. It is never printed and never in logs; URLs are logged without credentials or user.
- Host keys are trusted on first use, kept in `<data dir>/mirror_known_hosts`. If a remote's key changes, the mirror is paused (`host_key`). After checking the change is legitimate, remove that host's line from the file and run `ermite mirror sync <repo> --force`.
- `ermite mirror key rotate` (owner only) makes a new key at once. Every mirror fails until the new key is added to the remotes.
- The scheduler runs in the server process: run a single ermite instance per data directory.
