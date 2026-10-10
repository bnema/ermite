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
