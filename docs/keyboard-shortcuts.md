# Web keyboard shortcuts

Press `?` for the complete, page-specific list. Shortcuts do not run while typing
in a field. Section navigation uses two successive keys, not a chord:

| Keys | Destination |
| --- | --- |
| `g s` | Summary |
| `g f` | Files |
| `g c` | Commits |
| `g b` | Branches & tags |
| `g m` | Merge requests |
| `g i` | Issues |
| `g r` | Releases |
| `g a` | Actions (when CI is enabled) |
| `g h` | Home |
| `g g` | Top / first row |
| `G` (Shift+g) | Bottom / last row |

There is no `g t` alias, and `s`, `t`, `c`, `r`, `m`, and `i` alone do
not navigate sections. `b` still opens the branch/tag switcher; `f`, `:`,
and Ctrl+K (Cmd+K on macOS) open the command palette.

On merge requests, `l` focuses the files side panel; `g f` always navigates
to the repository Files section. `h` returns to the content. Uppercase `M`
opens the merge menu, `R` marks the head read, `E` expands every diff file,
and `I` opens the signed-in owner's inbox.

The help dialog can turn off character shortcuts, including `g` sequences
and shifted letters. Ctrl+K / Cmd+K and Escape remain available.
