// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import "bnema.dev/ermite/internal/domain"

// AuthorOf exposes authorOf to the tests.
func AuthorOf(p domain.Principal) string { return authorOf(p) }
