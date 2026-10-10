// SPDX-License-Identifier: AGPL-3.0-or-later

package githttp

// MaxTracked is the bound on the addresses a Limiter remembers.
const MaxTracked = maxTracked

// Tracked returns how many clients have failures on record.
func (l *Limiter) Tracked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.failures)
}
