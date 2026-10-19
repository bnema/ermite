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
