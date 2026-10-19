# Merge requests

A merge request proposes a branch for a target (the default branch unless set).

```sh
ermite mr create myrepo --source feature --title "Add feature"
ermite mr show myrepo 1 --diff
ermite mr merge myrepo 1 --head <commit> [--squash] [--delete-branch]
```

- **Fast-forward** moves the target. **Squash** writes one commit signed by the server's SSH key.
- The merge is refused when the branch moved since the reviewed `--head`, or when the target is not an ancestor of it.
- With `failing_checks_block` on (default), the merge is refused while a check of the head failed or is still running.
- `--delete-branch` deletes the source branch, unless it is the default branch or another open request uses it.
- `ermite repo edit --merge-modes` limits the modes per repository.
- Scopes: `mr:write` to open and comment, `mr:review` to give a verdict, `mr:merge` to merge.

Verify squash commits with the `allowed_signers` line from `ermite signing-key show`:

```sh
git verify-commit <commit>
```

## Notes

A note is the only review object. It is published as soon as it is written: there is no draft and no submit step.

```sh
ermite mr comment myrepo 1 -m "General remark"
ermite mr comment myrepo 1 -m "Why?" --path main.go --line 10-14 --side new
ermite mr comment myrepo 1 -m "Fixed in ^3" --reply 2
ermite mr resolve myrepo 1 2
ermite mr edit-comment myrepo 1 2 -m "New text"
ermite mr delete-comment myrepo 1 2
```

- Notes are Markdown, up to 8000 characters, rendered without raw HTML. Quote with `> `.
- `^N` in a note or a description links to note N of the same merge request. A reference to a missing note stays plain text.
- A note is general, on one line, or on a range `A-B` of lines on one side (`old` or `new`) of the diff. The lines must be in the diff.
- Threads have one level: a reply (`--reply <id>`) takes the anchor of its root and cannot carry a verdict. Resolving or reopening is done on the root and covers its replies.
- Only the author edits or deletes a note. An edited note shows "edited". A deleted note loses its text; a deleted root with replies stays as "deleted note".

## Verdicts

A root note can carry a verdict: `--approve` or `--request-changes`. The text may then be empty. A verdict needs the `mr:review` scope (or the owner); `mr:write` comments but cannot give one.

- A verdict ends a review: agents and `ermite watch` act on it, not on each note.
- Verdicts are informative: none blocks or allows a merge.
- `mr show` lists the latest verdict of each author (`verdicts` in `--json`).
- An approval is stale once the branch head differs from the head it was given on. Changes requested stay current until their author gives a new verdict, whatever is pushed.
- Verdicts are immutable: editing or deleting the note keeps its verdict. Only a new verdict of the same author replaces it.
