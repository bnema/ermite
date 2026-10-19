# Merge requests

A merge request proposes a branch for a target (the default branch unless set).

```sh
ermite mr create myrepo --source feature --title "Add feature"
ermite mr show myrepo 1 --diff
ermite mr merge myrepo 1 --head <commit> [--mode fast-forward|squash|merge] [--delete-branch]
```

`ermite mr create` and `ermite mr close` print a confirmation followed by the full
merge request web URL on its own line. With `--json`, merge request objects
(create, show, list, close and merge) include `url` alongside the other fields.

## Merge modes

| Mode | Result | Needs |
|---|---|---|
| `fast-forward` (default) | the target moves to the head, no commit | the target is an ancestor of the head |
| `squash` | one new commit with the merged tree, the target tip as only parent | the branch merges without conflicts |
| `merge` | a merge commit with two parents, the target tip and the head | the branch merges without conflicts |

- The server never rebases. A branch behind its target merges with `squash` or `merge`, or is rebased and pushed again for a fast-forward.
- The server checks the mode again when it merges, whatever the badge showed, and refuses with the reason (the target moved, a conflict with the paths).
- The merge is also refused when the branch moved since the reviewed `--head`.
- With `failing_checks_block` on (default), the merge is refused while a check of the head failed or is still running.
- The merge answers as soon as the target has moved. `--delete-branch` then deletes the source branch right after, in the background (unless it is the default branch or another open request uses it); if the branch moved meanwhile, it is kept.
- Every mode is allowed by default, including in repositories that had only `fast-forward` and `squash` before `merge` existed: those gain `merge`. Disable a mode per repository with `ermite repo edit <repo> --merge-modes fast-forward,squash` (list the modes to keep).
- Scopes: `mr:write` to open and comment, `mr:review` to give a verdict, `mr:merge` to merge.

## Signed commits

Squash and merge commits are signed by ermite's own key. The key is generated automatically the first time the server starts, belongs to the server, and its committer identity is `ERMITE_COMMITTER_NAME` / `ERMITE_COMMITTER_EMAIL`. `ermite signing-key show` prints its fingerprint and the `allowed_signers` line; use it to verify locally, or register the public key on GitHub or Forgejo.

```sh
ermite signing-key show --allowed-signers > allowed_signers
git -c gpg.format=ssh -c gpg.ssh.allowedSignersFile=allowed_signers verify-commit <commit>
```

The merge box of the web page and the output of `mr merge --mode squash|merge` repeat this with the fingerprint.

## Mergeability

The server checks in the background whether each open request merges with its target, and again whenever the source or the target moves. `mr show` prints it; the web merge box shows it as a live badge; `ermite watch` reports each change.

| State | Meaning | Modes possible |
|---|---|---|
| `checking` | no result yet for the current branches | none (wait) |
| `up_to_date` | the target is an ancestor of the head | all |
| `behind` | the head lacks `behind` commits of the target (`behind_capped`: at least 1000) but merges without conflicts | `squash`, `merge` |
| `conflict` | the merge conflicts; up to 20 `conflict_paths` are listed (`conflict_paths_truncated` when there are more); no paths means no common history | none (rebase) |

```sh
ermite mr show myrepo 1
# mergeability  behind by 2, merges without conflicts (squash or merge commit; fast-forward needs a rebase)
```

In `mr show --json`, `mergeability` holds `state`, `behind`, `behind_capped`, `conflict_paths`, `conflict_paths_truncated`, `target_id` and `head_id`; it is `null` unless the request is open. `ermite watch` prints `merge_request_mergeability` events when the state changes (not on every push):

```text
2026-10-19T14:00:00Z  merge_request_mergeability  myrepo!1 mergeability: behind by 2
```

In the web merge box a mode that is not possible is disabled with its reason, and the Merge button too when no mode is possible. A merge the server refuses (target moved, conflict, branch moved) shows the page again with the reason in the box.

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

## Shared read position and history queries

The read position is shared, not per principal. `MergeRequestDetail.HeadUnread`
is a fact: the nonzero head differs from that position. The web presentation
still decides who sees the mark-read button.

Repository comparisons return at most `ports.MaxCompareCommits` (500) commits.
Push summaries count at most `ports.MaxPushSummaryCommits` (100) commits and
report whether more exist; ancestry, rather than the bounded list, detects a
rewrite. Merge-request details include at most `ports.MaxMergeRequestCommits`
(100) commits. These history queries remain on the merge-request service
because it owns the merge-git port.
