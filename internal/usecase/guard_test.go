// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"bnema.dev/zerowrap"
	"github.com/stretchr/testify/require"
)

func TestLogPanic(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value any
		want  string
	}{
		{"string", "x", "x"},
		{"error", errors.New("panic error"), "panic error"},
		{"number", 42, "42"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			log := zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &buf})
			returned := false
			func() {
				defer logPanic(log.WithField("worker", "w"), "boom work")
				panic(tc.value)
			}()
			returned = true
			require.True(t, returned)
			require.Equal(t, 1, bytes.Count(buf.Bytes(), []byte("\n")))
			var entry map[string]any
			require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
			require.Equal(t, "boom work", entry["message"])
			require.Equal(t, tc.want, entry["panic"])
			require.Equal(t, "w", entry["worker"])
			require.NotEmpty(t, entry["stack"])
		})
	}
	t.Run("without panic", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		log := zerowrap.New(zerowrap.Config{Level: "debug", Format: "json", Output: &buf})
		func() { defer logPanic(log, "boom work") }()
		require.Empty(t, buf.String())
	})
}
