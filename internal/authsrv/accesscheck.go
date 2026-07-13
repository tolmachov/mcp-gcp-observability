package authsrv

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// projectAccessPermission is the permission probed to decide whether an
// account "has access to the project". Every predefined viewer-style role
// (roles/viewer, logging.viewer, monitoring.viewer, ...) contains it, so any
// principal with at least one read role on the project passes.
const projectAccessPermission = "resourcemanager.projects.get"

// AccessChecker decides whether the bearer of a Google access token may use
// this server. Production uses the Cloud Resource Manager testIamPermissions
// probe (see Config.RequireProjectAccess); tests inject fakes.
type AccessChecker interface {
	// HasProjectAccess reports whether the token's account holds
	// projectAccessPermission on the given project. "No access" is
	// (false, nil); the error return is for transient failures only, where
	// access could not be determined.
	HasProjectAccess(ctx context.Context, googleAccessToken, project string) (bool, error)
}

// crmAccessChecker probes Cloud Resource Manager with the user's own access
// token: testIamPermissions requires no permission itself and returns the
// subset of the asked-for permissions the caller actually holds.
type crmAccessChecker struct {
	endpoint string // overridable in tests
	client   *http.Client
}

func newCRMAccessChecker() *crmAccessChecker {
	return &crmAccessChecker{
		endpoint: "https://cloudresourcemanager.googleapis.com",
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *crmAccessChecker) HasProjectAccess(ctx context.Context, googleAccessToken, project string) (bool, error) {
	body := `{"permissions":["` + projectAccessPermission + `"]}`
	// Not attacker-influenced: endpoint is a constant (a test server in
	// tests) and project is either server configuration or a
	// format-validated user choice (see projectIDRe), path-escaped here.
	reqURL := c.endpoint + "/v3/projects/" + url.PathEscape(project) + ":testIamPermissions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(body)) //nolint:gosec // G704: see above
	if err != nil {
		return false, fmt.Errorf("building testIamPermissions request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+googleAccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req) //nolint:gosec // G704: see above
	if err != nil {
		return false, fmt.Errorf("calling testIamPermissions: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			Permissions []string `json:"permissions"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return false, fmt.Errorf("decoding testIamPermissions response: %w", err)
		}
		return slices.Contains(out.Permissions, projectAccessPermission), nil
	case http.StatusForbidden, http.StatusNotFound:
		// CRM answers 403/404 when the caller cannot even see the project;
		// that is a definitive "no access", not a transient failure.
		return false, nil
	default:
		return false, fmt.Errorf("testIamPermissions: unexpected status %s", resp.Status)
	}
}
