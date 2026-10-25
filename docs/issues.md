# Issues

An issue records a task, a bug or a question about a repository: a title, a body, comments, and an open, closed or resolved state. Issues share the numbering of merge requests: after merge request 1, the next issue is number 2.

```sh
ermite repo edit myrepo --issues owner
printf '%s\n' "It crashes." | ermite issue create myrepo --title "Crash on start" --body-file -
ermite issue list myrepo [--state open|closed|resolved|all]
ermite issue show myrepo 2
ermite issue comment myrepo 2 -m "Reproduced."
ermite issue resolve myrepo 2
ermite issue close myrepo 2
ermite issue reopen myrepo 2
```

`ermite issue create` prints a confirmation followed by the full issue web URL on its own line
(`issue close` and `issue reopen` do the same). With `--json`, issue objects include `url`.

All commands take `--json`. `--body-file -` reads standard input; `--body-file PATH` reads a file (1 MiB limit). A server needs the `issues` feature (`ermite version`); without it the commands, `repo edit --issues` and `token create --scope issue:write` are refused.

## States and history

- **Open**: work remains.
- **Closed**: closed without being resolved.
- **Resolved**: fixed, either with `ermite issue resolve <repo> <n>` or by merging a closing reference.

The web list has Open, Closed and Resolved tabs. The web offers Close for open issues and Reopen for closed or resolved issues; manual resolution is CLI-only. Reopening clears the resolving merge request. A merge does not resolve an already closed issue. Existing issues closed by a merge are migrated to resolved without adding historical events.

The issue conversation interleaves comments and timestamped system rows for references, closure, reopening and resolution, with actor and merge request links. History is append-only: removing a current reference does not remove its history. At most 500 events are retained per issue; later events are omitted without preventing state changes or merges.

Comments are Markdown, up to 8000 characters, rendered like merge request notes: no raw HTML, and `#N`, `!N` and commit SHAs become links. In the web, every Markdown field has a formatting toolbar (Ctrl/Cmd+B, I, K), continues lists on Enter and highlights the Markdown as you type.

## The `issues` setting

`ermite repo edit <repo> --issues off|owner|public`, shown by `repo show` (JSON field `issues`):

- `off` (default): issues are disabled.
- `owner`: the owner and tokens with `issue:write` write issues.
- `public`: anyone may propose an issue through a moderated form (see [Public form and moderation](#public-form-and-moderation)). The repository must be public too.

## Who can write

The owner, and tokens with the `issue:write` scope. For an agent that proposes changes on its own branches and reports issues:

```sh
ermite token create claude --preset agent --scope issue:write --repo <repo> --branch-prefix agent/<name>/
```

## Numbering

Merge requests and issues of a repository draw `#n` from one sequence, so a number names one thing: `ermite mr show myrepo 1`, `ermite issue show myrepo 2`.

## Merge request links

A merge request and the issues of its repository are linked by `#N` in its text:

- **Closing keywords** resolve an open issue when the merge request is **merged**, and only then: `close`, `closes`, `closed`, `fix`, `fixes`, `fixed`, `resolve`, `resolves`, `resolved` (any case), then `#N`, with an optional `:` after the keyword. A list works: `fixes #3, #4 and #5`. They count in the title, the description and the commit messages, not in notes. Closing the merge request without merging resolves nothing.
- **Any `#N`** in the title, description, notes or commit messages shows the merge request on the issue (`ermite issue show`, the API `references`), with its state and whether it resolves the issue on merge.
- A merge-resolved issue records the merge request that resolved it ("resolved by !N"). Reopening clears it.
- The links are recomputed whenever the merge request changes (a note added, edited or deleted, a push to its branch, a merge), so removing a mention removes the link.
- Words glued to the number (`abc#3`), `&#39;` and `##3` are not references. Code spans are not skipped.

Limits: the 100 most recent commits of the merge request, 16 KiB of each text, 100 merge requests listed per issue. Only numbers of existing issues are linked: a mention of a number that does not exist yet is not filled in later. With issues `off`, nothing is resolved.

## Inbox

The signed-in owner has an Inbox at `/-/inbox`, reached from the **Inbox** button at the top left of the home page, which shows the number of open items. It lists the open merge requests and the open issues of every repository, most recently updated first. The counters at the top (`N merge requests · M issues`) always count everything; `?repo=<name>` restricts the two lists to one repository, and the repository links under the counters set it. Repositories with issues off contribute no issues.

The **Running** section, after **Awaiting moderation**, lists queued and in-progress CI runs newest first, with links to each run, its repository, ref, and start time. The `N running` counter counts every repository; the repository filter also applies to this section. **Shift+I** (`I`) opens the inbox from any page for the signed-in owner, unless single-key shortcuts are off; `g i` opens the issues tab.

The **Activity** section, last on the page, is a journal of the last 24 hours (`?range=24h`, the default) or 7 days (`?range=7d`), newest first: merge requests opened, merged or closed, issues opened or closed, releases published, tags created or deleted, and pushes to a repository's default branch with their commit count. Pushes to other branches and CI runs are not listed. Above it, a line per repository whose default branch has commits after its nearest tag, such as `ermite-api — 4 commits since v0.12.0 · 3d ago` (counted up to 500); repositories without a tag are not listed. The repository filter applies to both. Entries are kept 30 days.

The page updates live: run status changes, merge request and issue events of any repository refresh the counters and the lists. Anyone who is not the signed-in owner, including a token, gets the same 404 as for an unknown page, and the same answer for the `owner` event stream it follows.

## Public form and moderation

With `issues` set to `public` on a public repository, a visitor who is not signed in finds a **New issue** button on the Issues tab, which opens `/<repo>/issues/new`. The form needs JavaScript: the page solves a short proof of work in a worker while the visitor writes (a second or so; harder after many recent submissions), and the button stays disabled until it is done and a few seconds have passed. Without JavaScript the form stays hidden and the page says why. A repository that is private, or whose mode is `off` or `owner`, has no such page: it answers the same 404 as an unknown one.

Limits: a title of at most 120 characters, a description of at most 10,000, an optional name of at most 40 (not verified). Each network may submit 3 issues per hour and 10 per day. At most 200 submissions await moderation; beyond that the form says submissions are paused. Before the proof of work is valid, nothing reveals whether a network is banned or limited, or whether the queue is full: every such failure gets the same generic refusal. A submission never appears on its own: it is stored as pending and shows no id to the visitor.

The owner moderates in the Inbox, in the first section, **Awaiting moderation**, whose count is in the counters row. Each submission shows its repository, age, name (or "anonymous"), title and the start of the description, as plain text, with three buttons: **Approve** opens it as the next issue of the repository (numbers are shared with merge requests), **Reject** deletes it, **Reject and ban** deletes it and refuses further submissions from the same network for 30 days. The section updates live when a submission arrives.

An approved issue is rendered as untrusted Markdown: raw HTML is dropped, images and videos are never loaded (image alt text is shown), only `http`, `https` and `mailto` links are kept, each with `rel="nofollow noopener noreferrer ugc"`, and every other link loses its link and keeps its text.

No IP address is written to disk. The rate limiter keys on the normalized network address (the IPv4 address, or the IPv6 /64) for at most 24 hours and keeps it only in memory. Pending submissions and bans store only a keyed hash (HMAC) of that network, made with a server key.

The limits see the address of the client only if `ERMITE_TRUSTED_PROXIES` lists your reverse proxy; otherwise every visitor shares the proxy's address and one budget (see [configuration](configuration.md)).

## Edit text

`ermite issue edit myrepo 2 --title "Revised title" --body-file issue.md` updates only supplied fields. `--body-file -` reads stdin; an empty file clears the body. At least one flag is required; inline body text is not accepted. Titles have at most 120 characters and bodies at most 10,000. The command needs `issue:write` and supports `--json`. Open, closed and resolved issues can be edited; pending submissions cannot. Identical values preserve `updated_at` and emit no event. History records `edited` with `changed_fields` naming `title` and/or `body`.

Text edits publish dedicated `issue_edited` and `merge_request_edited` live events, not comment events. `ermite watch` shows the repository, item number and `changed_fields` (`title`/`body` or `title`/`description`); unknown event kinds are ignored.

Issue history retains at most 500 entries. Once full, edits still persist, update `updated_at` and publish live events, but no new history entry is retained.

Issue and merge request creation accept body/description text only from files: `issue create <repo> --title T [--body-file PATH|-]` and `mr create <repo> --source B --title T [--description-file PATH|-]`. Both file flags are optional; omitting them creates empty text. `-` reads stdin, with a 1 MiB limit shared with edit commands. Inline `--body` and `--description` are not accepted for these commands. Repository descriptions remain inline.
