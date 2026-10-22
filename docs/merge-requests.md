# Merge requests

A merge request proposes a branch for a target (the default branch unless set).

```sh
ermite mr create myrepo --source feature --title "Add feature"
ermite mr show myrepo 1 --diff
ermite mr merge myrepo 1 --head <commit> [--mode fast-forward|squash|merge] [--delete-branch]
```

Naming an issue with `#N` links it, and `Fixes #N` resolves it on merge: see [issues](issues.md#merge-request-links).

`ermite mr create` and `ermite mr close` print a confirmation followed by the full
merge request web URL on its own line. With `--json`, merge request objects
(create, show, list, close and merge) include `url` alongside the other fields.

A draft cannot merge. `ermite mr ready <repo> <n>` removes the `WIP:` or `[WIP]` title prefix; it needs `mr:write` and an open draft.

## Linked issues and history

The web Issues rail lists current linked issues with their state and a **resolves** marker for closing references. Links lead to the issue. The conversation includes timestamped system rows for references and resolutions, interleaved with general comments. Removing a reference removes its current link, not its history; historical rows may name issues no longer in the rail.

## Merge modes

| Mode | Result | Needs |
|---|---|---|
| `fast-forward` (default) | the target moves to the head, no commit | the target is an ancestor of the head |
| `squash` | one new commit with the merged tree, the target tip as only parent | the branch merges without conflicts |
| `merge` | a merge commit with two parents, the target tip and the head | the branch merges without conflicts |

- The server never rebases. A branch behind its target merges with `squash` or `merge`, or is rebased and pushed again for a fast-forward.
- The server checks the mode again when it merges, whatever the badge showed, and refuses with the reason (the target moved, a conflict with the paths).
- The merge is also refused when the branch moved since the reviewed `--head`.
- A merge needs passing checks on the head: no failed, queued or running checks. A head with no checks can merge. `--ignore-checks` bypasses this, needs `mr:force-merge`, and cannot be combined with `--auto` or `--cancel-auto`. The owner web merge form offers **Ignore checks** when checks fail or are pending.
- The merge answers as soon as the target has moved. `--delete-branch` then deletes the source branch right after, in the background (unless it is the default branch or another open request uses it); if the branch moved meanwhile, it is kept.
- Every mode is allowed by default, including in repositories that had only `fast-forward` and `squash` before `merge` existed: those gain `merge`. Disable a mode per repository with `ermite repo edit <repo> --merge-modes fast-forward,squash` (list the modes to keep).
- Scopes: `mr:write` to open and comment, `mr:review` to give a verdict, `mr:merge` to merge, `mr:force-merge` to bypass checks. The `cli` preset includes the bypass scope; `agent` does not. Adding `--scope mr:merge` lets an agent merge only with passing checks.

## Signed commits

Squash and merge commits are signed by ermite's own key. The key is generated automatically the first time the server starts, belongs to the server, and its committer identity is `ERMITE_COMMITTER_NAME` / `ERMITE_COMMITTER_EMAIL`. `ermite signing-key show` prints its fingerprint and the `allowed_signers` line; use it to verify locally, or register the public key on GitHub or Forgejo.

```sh
ermite signing-key show --allowed-signers > allowed_signers
git -c gpg.format=ssh -c gpg.ssh.allowedSignersFile=allowed_signers verify-commit <commit>
```

The merge box of the web page and the output of `mr merge --mode squash|merge` repeat this with the fingerprint.

## Auto-merge

A merge request can merge by itself once its checks passed.

- The repository setting `allow_auto_merge` is off by default: `ermite repo edit <repo> --allow-auto-merge` (`=false` to turn it off).
- Enable it with `ermite mr merge <repo> <n> --head <commit> --auto --mode squash` (`--head` is required, as for a plain merge; a head that moved is refused with "branch moved"), or with **Merge when ready** in the web merge box. Its separate mode choice offers every repository-allowed mode, even while the head conflicts or is behind. Auto-merge keeps the source branch; the delete-source checkbox applies only to an immediate merge. It needs `mr:merge` and a server with CI. Re-enabling replaces the previous mode and enabler.
- The server merges when CI has settled on the head with every run complete and at least one check passed. A failing or unfinished check always blocks. The mode must still be allowed.
- The merge is made as the person who enabled it. Their token is checked at every evaluation, even while checks are pending, and again at merge time under the merge lock: it must still be valid and allow `mr:merge` on the repository and the target branch. A token that no longer qualifies cancels the auto-merge.
- A pending auto-merge shows in `mr show` (`auto_merge`), in the web merge box and as `merge_request_auto_merge_enabled` in `ermite watch`. A merge request merged this way has `auto_merged` true.
- A failing, unfinished or conflicting head waits with auto-merge still on. A re-run or a target fix can make it pass; pushing a new source head cancels the recorded intent (the reason names the new head).
- A manual merge supersedes auto-merge and publishes `merge_request_merged` with `auto=false`, without a cancelled event. A manual close clears it and publishes `merge_request_closed` followed by `merge_request_auto_merge_cancelled` with reason `closed`.
- Cancellation reasons are `cancelled by <author>`, `closed`, `auto-merge was turned off in the repository settings`, `the <mode> merge mode is no longer allowed`, `the source branch was deleted`, `the branch moved to <short-head>`, and `the principal that enabled auto-merge can no longer merge <target>`. If a change wins the merge lock after the evaluator's first read, the merge-time reason is instead `auto-merge is off in <repo>`, `the <mode> merge mode is not allowed in <repo>`, `the source branch "<source>" no longer exists`, or `branch moved, read again: "<source>" is at <head>, not <recorded-head>`. Other permanent merge-time preconditions report `!<n> is <state>`, `!<n> is a draft: mark it ready for review first`, `the target branch "<target>" no longer exists`, `nothing to merge, "<target>" already is at <head>`, or `nothing to merge, "<target>" already contains <head>`.
- Cancel it with `ermite mr merge <repo> <n> --cancel-auto` or **Cancel auto-merge** in the web merge box.

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

## Large diffs on the web

The merge request, commit and compare pages list every changed file. Generated files (lockfiles, `vendor/`, `node_modules/`, minified or generated code) and large files (over 2,000 changed lines) are collapsed: the header and the counts show, the lines do not. "load diff" fetches that one file in place, in full, and you can comment on its lines. Without scripts the link reloads the page with that file loaded. After the first 25 files of a merge request, the other files load as you scroll.

A commit page reads the hunks of its first 300 files with the page; the other files are listed collapsed and load one at a time. A file that cannot be shown in the browser, over 8 MiB of text or over 50,000 changed lines when loaded alone, has no "load diff": the page links to its raw blob instead, and its lines take no comment.

The API and `mr show --diff --json` keep their limits (300 files, 2,000 changed lines per file, 20,000 in total, 8 MiB). A file read without its hunks has `truncated` set and `omitted` giving the reason: `large`, `budget` (the earlier files used the limits) or `unavailable` (over 8 MiB of text, or a line over 64 KiB, even alone). Files past the 300th are not listed and `diff_truncated` is set.

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

### Review page panels

The main request card ends with a footer of two tabs, Commits and Checks, both
collapsed by default. Clicking a tab opens its panel, clicking the open tab
folds it again, clicking the other tab switches. Arrow keys (and Home / End)
move between the tabs. Without scripts the tabs are anchor links and the URL
fragment (`#mr-commits`, `#mr-checks`) shows the panel. The checks summary above
the diff opens the Checks panel and continues to update live. Commit totals use
`N+` when the branch list is capped: the reader's existing count operation counts all reachable
ancestors, not the source-minus-target range.

The rail keeps Files (including its `l` shortcut) and Verdicts. Merge settings
are read-only information in the More (⋯) menu, available to every viewer.
Signing-key information is also there when a merge box offers signed modes;
merge actions remain restricted to their existing permissions.

## Live source-branch updates

A push to the source branch refreshes the web review in place: the diff, files
rail, commits/checks footer, diff range and merge controls follow the new head.
The review keeps its visible file/line, folded files and open footer panel.
The page request still reads only one 25-file window of hunks; the visible
anchor's window is fetched too if needed, and other windows remain lazy.

While a comment is focused or has unsent edits, the refresh waits. A small
“branch updated — refreshing after your comment” status explains the delay.
Sending, cancelling, or reverting the draft and leaving its form lets the
pending refresh proceed. Refresh never preserves or discards an unsent draft.
