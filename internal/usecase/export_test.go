// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"context"

	"bnema.dev/ermite/internal/domain"
)

// AuthorOf exposes authorOf to the tests.
func AuthorOf(p domain.Principal) string { return authorOf(p) }

// Sweep exposes the daily sweep to the tests.
func (c *CI) Sweep(ctx context.Context) { c.sweep(ctx) }
