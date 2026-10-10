// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import "html/template"

// View models handed to the templ components. Components only loop and
// branch on these; the handlers build them from the API messages.

// Crumb is one segment of the path in the top bar.
type Crumb struct {
	Label   string
	Href    string
	Current bool
}

// Section is a repository section in the top bar, reached by its key.
type Section struct {
	Key     string // mnemonic key
	Post    string // rest of the label after the key
	Href    string
	Current bool
}

// Status is the content of the bottom bar.
type Status struct {
	Mode    string // "home", "summary", "refs"…
	Context string // "3 repos · sort push ▼"
	Hints   string // "j/k move · enter open · ? help"
	Cursor  bool   // show the row cursor position n/N
	Blob    bool   // show the selected line range
	Count   int    // number of rows under the cursor
}

// RefItem is a branch or tag in the ref switcher.
type RefItem struct {
	Name    string
	Href    string
	Current bool
}

// CmdItem is one target of the ":" command line.
type CmdItem struct{ Kind, Label, Href string }

// Layout is the input of the page shell.
type Layout struct {
	Title     string
	Instance  string
	Version   string
	SourceURL string
	Assets    Assets
	Crumbs    []Crumb
	Ref       string // ref shown in the top bar, "" outside a repo view
	Short     string // short sha shown after the ref
	Sections  []Section
	Status    Status
	Repo      string
	SHA       string // full sha of the ref, for the permalink key
	RefPath   string // the URL segment that `y` rewrites: the ref name
	Heads     []RefItem
	Tags      []RefItem
	Cmd       []CmdItem
	HasRail   bool
	RailSum   string // the rail as one line when it folds above the content (< 1100px)
	Read      bool   // prose page: content capped at the reading measure
	// Live connects the status bar to the event stream (/-/events).
	Live bool
}

// Link is a plain label + href pair.
type Link struct{ Label, Href string }

// HomeRow is one repository in the home list.
type HomeRow struct {
	ID       string
	Name     string
	Href     string
	Desc     string
	Pushed   string
	PushedAt string // RFC 3339, "" when never pushed
	Branch   string
	Tag      string
	Cursor   bool
}

// SortCol is a column header of the home list; it links to its sort order.
type SortCol struct {
	Label string
	Href  string
	Dir   string // "ascending" | "descending" | "none"
	Mark  string
	Class string
}

// HomePage is the repository list.
type HomePage struct {
	Cols []SortCol
	Rows []HomeRow
}

// Fact is one key/value row of the facts block.
type Fact struct {
	Key, Val, ID, Time string
}

// Clone is a block of copyable shell lines, one [copy] button per line.
type Clone struct {
	ID    string
	Lines []string
}

// Heading is one README outline entry.
type Heading struct {
	Level int
	ID    string
	Text  string
}

// Readme is a rendered README. HTML comes from the Markdown renderer (raw
// HTML dropped, URLs filtered) or is escaped plain text.
type Readme struct {
	Name string
	HTML template.HTML
	// Note replaces the content when the file is not rendered (too large).
	Note string
}

// CommitRow is one commit in the commit table.
type CommitRow struct {
	ID       string
	Short    string
	Subject  string
	CHref    string
	Author   string
	When     string
	WhenAt   string
	Merge    bool
	Branches []string
	Tags     []string
	Cursor   bool
}

// SummaryPage is the repository summary.
type SummaryPage struct {
	Desc    string
	Facts   []Fact
	Clone   Clone
	Recent  []CommitRow
	Readme  *Readme
	Outline []Heading
	AllHref string
}

// RefRow is a branch or a tag in the refs table.
type RefRow struct {
	Name, Href, Short, CHref, Subject, When, WhenAt string
	Default, Cursor                                 bool
}

// RefsPage lists the branches and tags of a repository.
type RefsPage struct {
	Branches []RefRow
	Tags     []RefRow
	Latest   string
	Default  string
	// Note says the lists were cut.
	Note string
}

// StatePage is a 404, 500 or empty-repository state.
type StatePage struct {
	Code  string // "404", "500", "empty"
	Title string
	Lines []string
	Clone *Clone
	Back  []Link
}
