package server

import (
	"context"
	"regexp"
	"strconv"
	"strings"
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

// TestPromptsRenderEveryDeclaredArgument sets every argument a prompt
// declares to a unique value and requires each value in the rendered text, so
// a render that reads a renamed or misspelled key fails here. It also pins
// that the numbered steps run 1, 2, 3, ... with and without the optional
// arguments.
func TestPromptsRenderEveryDeclaredArgument(t *testing.T) {
	step := regexp.MustCompile(`(?m)^(\d+)\. `)
	session := projectContractSession(t, "")
	listed, err := session.ListPrompts(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, listed.Prompts, len(promptSpecs))
	for _, prompt := range listed.Prompts {
		t.Run(prompt.Name, func(t *testing.T) {
			all, required := map[string]string{}, map[string]string{}
			for _, arg := range prompt.Arguments {
				value := "value-of-" + strings.ReplaceAll(arg.Name, "_", "-")
				all[arg.Name] = value
				if arg.Required {
					required[arg.Name] = value
				}
			}
			for name, args := range map[string]map[string]string{"all arguments": all, "required only": required} {
				result, err := session.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: prompt.Name, Arguments: args})
				require.NoError(t, err, name)
				require.Len(t, result.Messages, 1)
				text, ok := result.Messages[0].Content.(*mcp.TextContent)
				require.True(t, ok)
				for arg, value := range args {
					assert.Contains(t, text.Text, value, "%s: argument %s is not rendered", name, arg)
				}
				for i, m := range step.FindAllStringSubmatch(text.Text, -1) {
					assert.Equal(t, strconv.Itoa(i+1), m[1], "%s: step numbering", name)
				}
			}
		})
	}
}
