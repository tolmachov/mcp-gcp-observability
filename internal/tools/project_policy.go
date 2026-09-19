package tools

import (
	"fmt"
	"regexp"
)

var projectIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

// ProjectPolicy is the single project boundary for every public MCP surface.
// A non-empty pinned project is never accepted from caller input; an unpinned
// deployment requires and validates project_id on every project-scoped call.
type ProjectPolicy struct {
	pinned string
}

func NewProjectPolicy(pinned string) (ProjectPolicy, error) {
	if pinned != "" && !projectIDPattern.MatchString(pinned) {
		return ProjectPolicy{}, fmt.Errorf("invalid GCP_DEFAULT_PROJECT %q", pinned)
	}
	return ProjectPolicy{pinned: pinned}, nil
}

func MustProjectPolicy(pinned string) ProjectPolicy {
	p, err := NewProjectPolicy(pinned)
	if err != nil {
		panic(err)
	}
	return p
}

func (p ProjectPolicy) Pinned() bool { return p.pinned != "" }

func (p ProjectPolicy) Project() string { return p.pinned }

func (p ProjectPolicy) Resolve(requested string) (string, error) {
	if p.Pinned() {
		if requested != "" {
			return "", fmt.Errorf("project_id is not accepted by a pinned deployment")
		}
		return p.pinned, nil
	}
	if requested == "" {
		return "", fmt.Errorf("project_id is required")
	}
	if !projectIDPattern.MatchString(requested) {
		return "", fmt.Errorf("invalid project_id %q", requested)
	}
	return requested, nil
}
