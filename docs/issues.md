# Issues

An issue records a task, a bug or a question about a repository: a title, a body, comments, and an open or closed state. Issues share the numbering of merge requests: after merge request 1, the next issue is number 2.

```sh
ermite repo edit myrepo --issues owner
ermite issue create myrepo --title "Crash on start" --body "It crashes."
ermite issue list myrepo [--state open|closed|all]
ermite issue show myrepo 2
ermite issue comment myrepo 2 -m "Reproduced."
ermite issue close myrepo 2
ermite issue reopen myrepo 2
```

All commands take `--json`. `--body -` and `--body-file -` read standard input. A server needs the `issues` feature (`ermite version`); without it the commands, `repo edit --issues` and `token create --scope issue:write` are refused.

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

## Inbox

The signed-in owner has an Inbox at `/-/inbox`, reached from the **Inbox** button at the top left of the home page, which shows the number of open items. It lists the open merge requests and the open issues of every repository, most recently updated first. The counters at the top (`N merge requests · M issues`) always count everything; `?repo=<name>` restricts the two lists to one repository, and the repository links under the counters set it. Repositories with issues off contribute no issues.

The page updates live: merge request and issue events of any repository refresh the counters and the lists. Anyone who is not the signed-in owner, including a token, gets the same 404 as for an unknown page, and the same answer for the `owner` event stream it follows.

## Public form and moderation

With `issues` set to `public` on a public repository, a visitor who is not signed in finds a **New issue** button on the Issues tab, which opens `/<repo>/issues/new`. The form needs JavaScript: the page solves a short proof of work in a worker while the visitor writes (a second or so; harder after many recent submissions), and the button stays disabled until it is done and a few seconds have passed. Without JavaScript the form stays hidden and the page says why. A repository that is private, or whose mode is `off` or `owner`, has no such page: it answers the same 404 as an unknown one.

Limits: a title of at most 120 characters, a description of at most 10,000, an optional name of at most 40 (not verified). Each network may submit 3 issues per hour and 10 per day. At most 200 submissions await moderation; beyond that the form says submissions are paused. Before the proof of work is valid, nothing reveals whether a network is banned or limited, or whether the queue is full: every such failure gets the same generic refusal. A submission never appears on its own: it is stored as pending and shows no id to the visitor.

The owner moderates in the Inbox, in the first section, **Awaiting moderation**, whose count is in the counters row. Each submission shows its repository, age, name (or "anonymous"), title and the start of the description, as plain text, with three buttons: **Approve** opens it as the next issue of the repository (numbers are shared with merge requests), **Reject** deletes it, **Reject and ban** deletes it and refuses further submissions from the same network for 30 days. The section updates live when a submission arrives.

An approved issue is rendered as untrusted Markdown: raw HTML is dropped, images are never loaded (their alt text is shown), only `http`, `https` and `mailto` links are kept, each with `rel="nofollow noopener noreferrer ugc"`, and every other link loses its link and keeps its text.

No IP address is written to disk. The rate limiter keys on the normalized network address (the IPv4 address, or the IPv6 /64) for at most 24 hours and keeps it only in memory. Pending submissions and bans store only a keyed hash (HMAC) of that network, made with a server key.

The limits see the address of the client only if `ERMITE_TRUSTED_PROXIES` lists your reverse proxy; otherwise every visitor shares the proxy's address and one budget (see [configuration](configuration.md)).
