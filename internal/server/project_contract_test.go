package server

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
	"github.com/tolmachov/mcp-gcp-observability/internal/tools"
)

func projectContractSession(t *testing.T, pinned string) *mcp.ClientSession {
	t.Helper()
	s := testServer(t)
	s.project = tools.MustProjectPolicy(pinned)
	s.completer.project = s.project
	srv := s.newMCPInstance(s.completer)
	s.registerResources(srv, tools.Deps{
		Logs:     stubBackends{},
		Errors:   stubBackends{},
		Traces:   stubBackends{},
		Registry: metrics.NewRegistry(),
		Project:  s.project,
	})
	s.registerPrompts(srv)
	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx, st) }()
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "contract", Version: "1"}, nil)
	session, err := mcpClient.Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestPinnedResourcesAreExactAndUnpinnedResourcesAreTemplates(t *testing.T) {
	pinned := projectContractSession(t, "pinned-project")
	resources, err := pinned.ListResources(context.Background(), nil)
	require.NoError(t, err)
	templates, err := pinned.ListResourceTemplates(context.Background(), nil)
	require.NoError(t, err)
	for _, uri := range []string{
		"gcp-logs://pinned-project/recent",
		"gcp-errors://pinned-project/groups",
		"gcp-traces://pinned-project/recent",
	} {
		assert.Contains(t, resourceURIs(resources.Resources), uri)
	}
	assert.Empty(t, templates.ResourceTemplates)

	unpinned := projectContractSession(t, "")
	resources, err = unpinned.ListResources(context.Background(), nil)
	require.NoError(t, err)
	templates, err = unpinned.ListResourceTemplates(context.Background(), nil)
	require.NoError(t, err)
	assert.NotContains(t, resourceURIs(resources.Resources), "gcp-logs:///recent")
	assert.ElementsMatch(t, []string{
		"gcp-logs://{project}/recent",
		"gcp-errors://{project}/groups",
		"gcp-traces://{project}/recent",
	}, resourceTemplateURIs(templates.ResourceTemplates))
}

func resourceURIs(resources []*mcp.Resource) []string {
	out := make([]string, 0, len(resources))
	for _, resource := range resources {
		out = append(out, resource.URI)
	}
	return out
}

func resourceTemplateURIs(resources []*mcp.ResourceTemplate) []string {
	out := make([]string, 0, len(resources))
	for _, resource := range resources {
		out = append(out, resource.URITemplate)
	}
	return out
}

func TestProjectScopedPromptsFollowProjectPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, pinned string
		wantProject  bool
	}{
		{name: "pinned", pinned: "pinned-project"},
		{name: "unpinned", wantProject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := projectContractSession(t, tc.pinned)
			result, err := session.ListPrompts(context.Background(), nil)
			require.NoError(t, err)
			for _, prompt := range result.Prompts {
				if prompt.Name == "generate-metrics-registry" {
					continue
				}
				found, required := false, false
				for _, arg := range prompt.Arguments {
					if arg.Name == "project_id" {
						found, required = true, arg.Required
					}
				}
				assert.Equal(t, tc.wantProject, found, "prompt %s", prompt.Name)
				assert.Equal(t, tc.wantProject, required, "prompt %s", prompt.Name)
			}
		})
	}
}

func TestPromptProjectPolicyCannotBeBypassed(t *testing.T) {
	pinned := projectContractSession(t, "pinned-project")
	_, err := pinned.GetPrompt(context.Background(), &mcp.GetPromptParams{
		Name: "service-health", Arguments: map[string]string{"project_id": "other-project"},
	})
	assert.ErrorContains(t, err, "unknown prompt argument")

	unpinned := projectContractSession(t, "")
	_, err = unpinned.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "service-health"})
	assert.ErrorContains(t, err, "project_id is required")
	result, err := unpinned.GetPrompt(context.Background(), &mcp.GetPromptParams{
		Name: "service-health", Arguments: map[string]string{"project_id": "allowed-project"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.Messages)
	text, ok := result.Messages[0].Content.(*mcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, text.Text, "GCP PROJECT: allowed-project")
}
