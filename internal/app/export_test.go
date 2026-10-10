// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// Idle reports whether nothing is pending or running: tests wait for it
// instead of sleeping.
func (m *Maintainer) Idle() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries) == 0
}
