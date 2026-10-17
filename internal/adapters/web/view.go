// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import "html/template"

// View models handed to the templ components. Components only loop and
// branch on these; the handlers build them from the use case results.

// Crumb is one segment of the path in the header.
type Crumb struct {
	Label   string
	Href    string
	Current bool
}

// Section is a repository tab under the header, reached by its key.
type Section struct {
	Key     string // mnemonic key
	Label   string
	Href    string
	Current bool
	// Count is shown after the label when it is not empty: the open merge
	// requests.
	Count string
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
	Title string
	// Owner is the owner's name (ERMITE_OWNER), shown when signed in.
	Owner     string
	Version   string
	SourceURL string
	Assets    Assets
	Crumbs    []Crumb
	Ref       string // ref shown in the header, "" outside a repo view
	Short     string // short sha shown after the ref
	Sections  []Section
	Repo      string
	SHA       string // full sha of the ref, for the permalink key
	RefPath   string // the URL segment that `y` rewrites: the ref name
	Heads     []RefItem
	Tags      []RefItem
	Cmd       []CmdItem
	HasRail   bool
	// Lede is a line above the content and the rail, so both start level.
	Lede string
	// Live connects the page to the event stream (/-/events).
	Live bool
	// LiveURL is the stream the page follows, with its query.
	LiveURL string
	// PathsURL is the list of the file paths of the commit the page shows
	// (/{repo}/paths/{sha}) and BlobBase the URL its file links start with;
	// both are "" when the page has no commit to search.
	PathsURL, BlobBase string
	// Passkeys tells that the passkey routes exist: the header then links to
	// them.
	Passkeys bool
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
	// Fresh marks a row that a push just updated (live updates only).
	Fresh bool
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
	// Checks is the combined state of the CI checks of the commit; empty when
	// it has none.
	Checks ChecksSummary
}

// SummaryPage is the repository summary.
type SummaryPage struct {
	Facts   []Fact
	Clone   Clone
	Recent  []CommitRow
	Readme  *Readme
	Outline []Heading
	AllHref string
	// Latest is the newest tag, nil when the repository has none.
	Latest *LatestTag
	// Actions is the recent runs of the rail, nil when the viewer cannot read
	// CI (or the server runs none).
	Actions *RecentActions
}

// RecentActions is the "Recent actions" block of the summary rail: the newest
// runs. With no run the block is in the page but hidden, so that the first
// run reaches it live.
type RecentActions struct {
	Runs    []RunRow
	AllHref string
}

// LatestTag is the newest tag of a repository, shown in the summary rail.
// Name is empty when there are too many tags to date them; Count and
// RefsHref still lead to the full list.
type LatestTag struct {
	Name, Href, Subject, When, WhenAt string
	Count                             int
	RefsHref                          string
}

// RefRow is a branch or a tag in the refs table.
type RefRow struct {
	Name, Href, Short, CHref, Subject, When, WhenAt string
	Default, Cursor                                 bool
	// Checks is the combined state of the CI checks of the commit.
	Checks ChecksSummary
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

// LoginPage is the confirmation of a login link: a form with one button.
type LoginPage struct {
	Title  string
	Lines  []string
	Action string // where the form posts
	Button string
}

// PasskeyLoginPage is the sign-in page: one button the script drives.
type PasskeyLoginPage struct {
	Title  string
	Lines  []string
	Button string
	// Begin and Finish are the endpoints the script posts to.
	Begin, Finish string
}

// PasskeyRow is one passkey on the passkeys page.
type PasskeyRow struct {
	Name string
	// Created and LastUsed are relative ("3d ago"); the At fields are the
	// RFC 3339 forms, "" when never.
	Created, CreatedAt   string
	LastUsed, LastUsedAt string
	Synced               bool
	// Remove is where the remove form posts.
	Remove string
}

// PasskeysPage lists the passkeys and adds one.
type PasskeysPage struct {
	Rows          []PasskeyRow
	Begin, Finish string
	// Full tells that no more passkeys can be added.
	Full bool
	Max  int
}
