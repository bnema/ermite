// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"fmt"
	"runtime/debug"

	"bnema.dev/zerowrap"
)

// logPanic, deferred, logs a panic of the goroutine with its stack and
// stops it from going further up. msg names the work that panicked.
// It must be the deferred function itself (defer logPanic(...)): recover
// only works there.
func logPanic(log zerowrap.Logger, msg string) {
	if r := recover(); r != nil {
		log.Error().Str("panic", fmt.Sprint(r)).Bytes("stack", debug.Stack()).Msg(msg)
	}
}
