// SPDX-License-Identifier: AGPL-3.0-or-later

package eventbus

// Subscribers returns the number of live subscribers.
func (b *Bus) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
