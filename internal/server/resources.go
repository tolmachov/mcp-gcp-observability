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
	configJSON, err := json.Marshal(map[string]any{
		"default_project":        cfg.DefaultProject,
		"logs_max_limit":         cfg.LogsMaxLimit,
		"errors_max_limit":       cfg.ErrorsMaxLimit,
		"metrics_registry_file":  cfg.MetricsRegistryFile,
		"metrics_registry_count": reg.Count(),
	})
	if err != nil {
		return fmt.Errorf("failed to marshal config resource during startup: %w", err)
	}

	srv.AddResource(
		&mcp.Resource{
			URI:         "config://project",
			Name:        "Project Configuration",
			Description: "Current GCP project configuration: default project ID and query limits",
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
	s.registerResourceTemplates(srv, client)
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
// The {project} segment defaults to the configured project when the client
// leaves it empty (e.g. gcp-logs:///recent).
func (s *Server) registerResourceTemplates(srv *mcp.Server, client *gcpclient.Client) {
	defaultProject := client.Config().DefaultProject

	addTemplate := func(uriTemplate, name, description string, fetch func(ctx context.Context, project string) (any, error)) {
		srv.AddResourceTemplate(
			&mcp.ResourceTemplate{
				URITemplate: uriTemplate,
				Name:        name,
				Description: description,
				MIMEType:    "application/json",
			},
			func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				project := projectFromURI(req.Params.URI, defaultProject)
				if project == "" {
					return nil, fmt.Errorf("no project in URI %q and no default project configured", req.Params.URI)
				}
				cctx, cancel := context.WithTimeout(ctx, resourceTemplateTimeout)
				defer cancel()
				data, err := fetch(cctx, project)
				if err != nil {
					return nil, fmt.Errorf("reading resource %q: %w", req.Params.URI, err)
				}
				payload, err := json.Marshal(data)
				if err != nil {
					return nil, fmt.Errorf("marshaling resource %q: %w", req.Params.URI, err)
				}
				return &mcp.ReadResourceResult{
					Contents: []*mcp.ResourceContents{{
						URI:      req.Params.URI,
						MIMEType: "application/json",
						Text:     string(payload),
					}},
				}, nil
			},
		)
	}

	addTemplate("gcp-logs://{project}/recent", "Recent Logs Summary",
		"Severity distribution, top errors and top services from recent logs for the given project.",
		func(ctx context.Context, project string) (any, error) {
			return gcpdata.SummarizeLogs(ctx, client.LoggingClient(), project, "", nil)
		})

	addTemplate("gcp-errors://{project}/groups", "Error Reporting Groups",
		"Current Error Reporting groups for the given project over the last 24 hours, by count.",
		func(ctx context.Context, project string) (any, error) {
			return gcpdata.ListErrors(ctx, client.ErrorsClient(), project, 24, 50, "", "")
		})

	addTemplate("gcp-traces://{project}/recent", "Recent Traces",
		"Traces from the last hour for the given project.",
		func(ctx context.Context, project string) (any, error) {
			now := time.Now()
			return gcpdata.ListTraces(ctx, client.TraceClient(), project,
				"", "", "", now.Add(-time.Hour), now, 50, "")
		})
}

// projectFromURI extracts the {project} segment (the URI host) from a templated
// resource URI such as gcp-logs://my-project/recent, falling back to the
// configured default when the host is empty.
func projectFromURI(uri, defaultProject string) string {
	if u, err := url.Parse(uri); err == nil && u.Host != "" {
		return u.Host
	}
	return defaultProject
}
