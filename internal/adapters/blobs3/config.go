// SPDX-License-Identifier: AGPL-3.0-or-later
package blobs3

import "net/http"

// Config supplies explicit credentials and endpoint settings; no ambient
// credential source is consulted.
type Config struct {
	Bucket, Prefix, Endpoint, Region, AccessKeyID, SecretAccessKey string
	ForcePathStyle                                                 bool
	// HTTPClient optionally overrides the transport (primarily for tests).
	HTTPClient *http.Client
}
