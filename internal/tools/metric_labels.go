package tools

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

// AvailableLabels lists the labels a metric exposes for filtering and
// grouping, split by Cloud Monitoring namespace.
type AvailableLabels struct {
	Metric          []string            `json:"metric,omitempty"`
	Resource        map[string][]string `json:"resource,omitempty"`
	// IncompleteTypes lists monitored resource types whose label keys could
	// not be fetched (GetResourceLabels RPC failed). When non-empty, the
	// Resource map is partial and consumers should not assume unlisted label
	// keys are absent.
	IncompleteTypes []string            `json:"incomplete_resource_types,omitempty"`
}

// availableLabelsFromDescriptor builds an AvailableLabels value from a metric
// descriptor the handler has already fetched.
func availableLabelsFromDescriptor(ctx context.Context, req *mcp.CallToolRequest, querier gcpdata.MetricsQuerier, project, metricType string, desc gcpdata.MetricDescriptorBasic) *AvailableLabels {
	result := &AvailableLabels{}
	for _, l := range desc.Labels {
		result.Metric = append(result.Metric, l.Key)
	}
	sort.Strings(result.Metric)

	if len(desc.MonitoredResourceTypes) > 0 {
		result.Resource = make(map[string][]string, len(desc.MonitoredResourceTypes))
		for _, rt := range desc.MonitoredResourceTypes {
			labels, rerr := querier.GetResourceLabels(ctx, project, rt)
			if rerr != nil {
				mcpLog(ctx, req, logLevelWarning, "metric_labels",
					fmt.Sprintf("GetResourceLabels failed for type %q (metric %q): %v", rt, metricType, rerr))
				result.IncompleteTypes = append(result.IncompleteTypes, rt)
				continue
			}
			if labels == nil {
				continue
			}
			sort.Strings(labels)
			result.Resource[rt] = labels
		}
		if len(result.Resource) == 0 {
			result.Resource = nil
		}
	}
	return result
}

// fetchAvailableLabels loads metric-level and resource-level labels for a
// metric type via a fresh ListMetricDescriptors RPC.
func fetchAvailableLabels(ctx context.Context, req *mcp.CallToolRequest, querier gcpdata.MetricsQuerier, project, metricType string) *AvailableLabels {
	filter := fmt.Sprintf(`metric.type = "%s"`, gcpdata.EscapeFilterValue(metricType))
	descs, err := querier.ListMetricDescriptors(ctx, project, filter, 1)
	if err != nil {
		mcpLog(ctx, req, logLevelWarning, "metric_labels",
			fmt.Sprintf("ListMetricDescriptors failed for %q in %q: %v", metricType, project, err))
		return nil
	}
	if len(descs) == 0 {
		return nil
	}
	desc := descs[0]
	basic := gcpdata.MetricDescriptorBasic{
		Labels:                 desc.Labels,
		MonitoredResourceTypes: desc.MonitoredResourceTypes,
	}
	return availableLabelsFromDescriptor(ctx, req, querier, project, metricType, basic)
}

var invalidFilterErrRe = regexp.MustCompile(`(?i)filter does not specify a valid combination`)

func isInvalidFilterError(err error) bool {
	if err == nil {
		return false
	}
	if st, ok := status.FromError(err); ok {
		if st.Code() != codes.InvalidArgument {
			return false
		}
		return invalidFilterErrRe.MatchString(st.Message())
	}
	return invalidFilterErrRe.MatchString(err.Error())
}

func enrichInvalidFilterError(ctx context.Context, req *mcp.CallToolRequest, querier gcpdata.MetricsQuerier, project, metricType, labelFilter string, origErr error) string {
	labels := fetchAvailableLabels(ctx, req, querier, project, metricType)
	if labels == nil {
		return fmt.Sprintf("Failed to query metric: %v\n\n(Could not fetch label descriptors for %q to suggest a fix.)", origErr, metricType)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Filter invalid for %s.\n", metricType)
	fmt.Fprintf(&b, "  Your filter: %s\n", labelFilter)
	fmt.Fprintf(&b, "  GCP said:    %v\n", origErr)
	b.WriteString("\nAvailable labels for this metric:\n")
	if len(labels.Metric) > 0 {
		fmt.Fprintf(&b, "  metric.labels:   %s\n", strings.Join(labels.Metric, ", "))
	} else {
		b.WriteString("  metric.labels:   (none)\n")
	}
	if len(labels.Resource) > 0 {
		types := make([]string, 0, len(labels.Resource))
		for t := range labels.Resource {
			types = append(types, t)
		}
		sort.Strings(types)
		for _, t := range types {
			fmt.Fprintf(&b, "  resource.labels: %s   (%s)\n", strings.Join(labels.Resource[t], ", "), t)
		}
	} else {
		b.WriteString("  resource.labels: (none)\n")
	}

	if hint := filterMisplaceHint(labelFilter, labels); hint != "" {
		b.WriteString("\nHint: " + hint + "\n")
	}

	return b.String()
}

var filterKeyRe = regexp.MustCompile(`(metric|resource)\.labels\.([A-Za-z_][A-Za-z0-9_]*)`)

func filterMisplaceHint(labelFilter string, labels *AvailableLabels) string {
	if labelFilter == "" || labels == nil {
		return ""
	}
	matches := filterKeyRe.FindAllStringSubmatch(labelFilter, -1)
	if len(matches) == 0 {
		return ""
	}

	metricKeys := make(map[string]bool, len(labels.Metric))
	for _, k := range labels.Metric {
		metricKeys[k] = true
	}
	resourceKeys := make(map[string]bool)
	for _, keys := range labels.Resource {
		for _, k := range keys {
			resourceKeys[k] = true
		}
	}

	var hints []string
	for _, m := range matches {
		ns, key := m[1], m[2]
		switch ns {
		case "resource":
			if !resourceKeys[key] && metricKeys[key] {
				hints = append(hints, fmt.Sprintf("`resource.labels.%s` does not exist on this metric's resource types — try `metric.labels.%s` instead.", key, key))
			}
		case "metric":
			if !metricKeys[key] && resourceKeys[key] {
				hints = append(hints, fmt.Sprintf("`metric.labels.%s` does not exist on this metric — try `resource.labels.%s` instead.", key, key))
			}
		}
	}
	if len(hints) == 0 {
		return ""
	}
	return strings.Join(hints, " ")
}
