// SPDX-License-Identifier: AGPL-3.0-or-later

package depsregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"bnema.dev/ermite/internal/domain"
)

// Malicious reports MAL- advisories for exactly this package version.
func (r *Registry) Malicious(ctx context.Context, eco domain.DepsEcosystem, name, version string) (bool, error) {
	var ecosystem string
	switch eco {
	case domain.EcosystemGoMod:
		ecosystem = "Go"
	case domain.EcosystemActions:
		ecosystem = "GitHub Actions"
	default:
		return false, domain.ErrInvalid
	}
	payload := struct {
		Package struct {
			Ecosystem string `json:"ecosystem"`
			Name      string `json:"name"`
		} `json:"package"`
		Version string `json:"version"`
	}{}
	payload.Package.Ecosystem = ecosystem
	payload.Package.Name = name
	payload.Version = version
	body, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.osv+"/v1/query", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	data, err := r.request(req, false)
	if err != nil {
		return false, err
	}
	var response struct {
		Vulns []struct {
			ID string `json:"id"`
		} `json:"vulns"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return false, fmt.Errorf("OSV response: %w", err)
	}
	for _, v := range response.Vulns {
		if strings.HasPrefix(v.ID, "MAL-") {
			return true, nil
		}
	}
	return false, nil
}
