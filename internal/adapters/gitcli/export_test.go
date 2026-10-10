// SPDX-License-Identifier: AGPL-3.0-or-later

package gitcli

import "sort"

// CatFilePIDs returns the process ids of the live long-lived cat-file
// processes, for the tests of the pool.
func (g *Git) CatFilePIDs() []int {
	g.cat.mu.Lock()
	defer g.cat.mu.Unlock()
	var pids []int
	for _, cp := range g.cat.procs {
		pids = append(pids, cp.cmd.Process.Pid)
	}
	sort.Ints(pids)
	return pids
}

// BlobStreams returns the number of blob streams that are open.
func (g *Git) BlobStreams() int {
	g.cat.mu.Lock()
	defer g.cat.mu.Unlock()
	return len(g.cat.streams)
}

// BlobStreamPIDs returns the process ids of the open blob streams.
func (g *Git) BlobStreamPIDs() []int {
	g.cat.mu.Lock()
	defer g.cat.mu.Unlock()
	var pids []int
	for s := range g.cat.streams {
		pids = append(pids, s.cmd.Process.Pid)
	}
	return pids
}

// FireIdleTimers runs, for every idle cat-file process, what its idle timer
// would do now, so that tests do not wait for real time.
func (g *Git) FireIdleTimers() {
	g.cat.mu.Lock()
	type armed struct {
		cp  *catProc
		gen uint64
	}
	var all []armed
	for _, cp := range g.cat.procs {
		all = append(all, armed{cp, cp.gen})
	}
	g.cat.mu.Unlock()
	for _, a := range all {
		g.cat.expire(a.cp, a.gen)
	}
}

// FireStaleIdleTimers runs what the idle timers armed before the last use of
// each process would do: nothing, since the process was used since.
func (g *Git) FireStaleIdleTimers() {
	g.cat.mu.Lock()
	type armed struct {
		cp  *catProc
		gen uint64
	}
	var all []armed
	for _, cp := range g.cat.procs {
		all = append(all, armed{cp, cp.gen - 1})
	}
	g.cat.mu.Unlock()
	for _, a := range all {
		g.cat.expire(a.cp, a.gen)
	}
}

// ExecSlots returns the size of the one-shot process semaphore and how many
// slots are taken.
func (g *Git) ExecSlots() (size, taken int) { return cap(g.exec), len(g.exec) }
