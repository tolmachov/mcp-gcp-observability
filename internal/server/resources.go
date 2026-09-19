package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
	"github.com/tolmachov/mcp-gcp-observability/internal/tools"
)

// registerResources adds MCP resources to srv.
func (s *Server) registerResources(srv *mcp.Server, client *gcpclient.Client, reg *metrics.Registry) error {
	cfg := client.Config()
	projectConfig := map[string]any{
		"project_mode":           "required",
		"metrics_registry_file":  cfg.MetricsRegistryFile,
		"metrics_registry_count": reg.Count(),
		"logs_hard_limit":        tools.LogsHardLimit,
		"errors_hard_limit":      tools.ErrorsHardLimit,
	}
	if s.project.Pinned() {
		projectConfig["project_mode"] = "pinned"
		projectConfig["project"] = s.project.Project()
	}
	configJSON, err := json.Marshal(projectConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal config resource during startup: %w", err)
	}

	srv.AddResource(
		&mcp.Resource{
			URI:         "config://project",
			Name:        "Project Configuration",
			Description: "Current project policy and hard response limits",
			MIMEType:    "application/json",
		},
		func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{{
					URI:      "config://project",
					MIMEType: "application/json",
					Text:     string(configJSON),
				}},
			}, nil
		},
	)

	tools.RegisterMetricsChartStaticResource(srv)
	tools.RegisterMetricsCompareChartStaticResource(srv)
	s.registerProjectResources(srv, client)
	return nil
}

// resourceTemplateTimeout bounds the GCP calls backing the navigable resource
// templates so a slow backend cannot hang a resources/read request.
const resourceTemplateTimeout = 30 * time.Second

// registerResourceTemplates adds URI-templated resources that let clients
// navigate a project's recent observability data like a filesystem:
//
//	gcp-logs://{project}/recent     — severity/error/service summary of recent logs
//	gcp-errors://{project}/groups   — current Error Reporting groups
//	gcp-traces://{project}/recent   — traces from the last hour
//
// Pinned deployments expose exact URIs; unpinned deployments expose templates
// whose project segment is mandatory.
func (s *Server) registerProjectResources(srv *mcp.Server, client *gcpclient.Client) {
	type spec struct {
		scheme, path, name, description string
		fetch                           func(context.Context, string) (any, error)
	}
	specs := []spec{
		{"gcp-logs", "/recent", "Recent Logs Summary",
			"Severity distribution, top errors and top services from recent logs for the given project.",
			func(ctx context.Context, project string) (any, error) {
				return gcpdata.SummarizeLogs(ctx, client.LoggingClient(), project, "", nil)
			}},
		{"gcp-errors", "/groups", "Error Reporting Groups",
			"Current Error Reporting groups for the given project over the last 24 hours, by count.",
			func(ctx context.Context, project string) (any, error) {
				return gcpdata.ListErrors(ctx, client.ErrorsClient(), project, gcpdata.ErrorWindow24H, 50, "", "")
			}},
		{"gcp-traces", "/recent", "Recent Traces",
			"Traces from the last hour for the given project.",
			func(ctx context.Context, project string) (any, error) {
				now := time.Now()
				return gcpdata.ListTraces(ctx, client.TraceClient(), project,
					"", "", "", now.Add(-time.Hour), now, 50, "")
			}},
	}
	for _, item := range specs {
		item := item
		handler := func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			project, err := projectFromResourceURI(req.Params.URI, item.scheme, item.path, s.project)
			if err != nil {
				return nil, err
			}
			cctx, cancel := context.WithTimeout(ctx, resourceTemplateTimeout)
			defer cancel()
			data, err := item.fetch(cctx, project)
			if err != nil {
				return nil, fmt.Errorf("reading resource %q: %w", req.Params.URI, err)
			}
			payload, err := json.Marshal(data)
			if err != nil {
				return nil, fmt.Errorf("marshaling resource %q: %w", req.Params.URI, err)
			}
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: string(payload)}}}, nil
		}
		if s.project.Pinned() {
			uri := fmt.Sprintf("%s://%s%s", item.scheme, s.project.Project(), item.path)
			srv.AddResource(&mcp.Resource{URI: uri, Name: item.name, Description: item.description, MIMEType: "application/json"}, handler)
		} else {
			template := fmt.Sprintf("%s://{project}%s", item.scheme, item.path)
			srv.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: template, Name: item.name, Description: item.description, MIMEType: "application/json"}, handler)
		}
	}
}

func projectFromResourceURI(raw, scheme, path string, policy tools.ProjectPolicy) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Scheme != scheme || u.Path != path || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("invalid resource URI %q", raw)
	}
	requested := u.Host
	if policy.Pinned() {
		if requested != policy.Project() {
			return "", fmt.Errorf("resource URI is outside the pinned project")
		}
		requested = ""
	}
	return policy.Resolve(requested)
}
