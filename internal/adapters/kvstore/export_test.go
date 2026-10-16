// SPDX-License-Identifier: AGPL-3.0-or-later

package kvstore

// SetAfterSettingsSnapshot installs the hook that runs between the snapshot of
// DeleteEnvironment and its batch, and returns the function that removes it.
func SetAfterSettingsSnapshot(f func()) (restore func()) {
	afterSettingsSnapshot = f
	return func() { afterSettingsSnapshot = nil }
}

// DecodeConcurrencyRecord, EncodeConcurrencyRecord and EncodeConcurrencyMark
// expose the stored layout of a concurrency group to the tests.
var (
	DecodeConcurrencyRecord = decodeConcurrencyRecord
	EncodeConcurrencyRecord = encodeConcurrencyRecord
	EncodeConcurrencyMark   = encodeConcurrencyMark
)
