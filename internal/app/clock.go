// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import "time"

// systemClock is the ports.Clock of the running server.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
