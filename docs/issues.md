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
- `public`: the anonymous moderated form; not available yet, the server refuses it.

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
