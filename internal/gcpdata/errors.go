package gcpdata

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"google.golang.org/api/iterator"

	errorreporting "cloud.google.com/go/errorreporting/apiv1beta1"
	"cloud.google.com/go/errorreporting/apiv1beta1/errorreportingpb"
)

const errorReportingTimeout = 30 * time.Second

// timeRangePeriod rounds up to the smallest Error Reporting query period that fully covers the requested range.
func timeRangePeriod(hours int) errorreportingpb.QueryTimeRange_Period {
	switch {
	case hours <= 1:
		return errorreportingpb.QueryTimeRange_PERIOD_1_HOUR
	case hours <= 6:
		return errorreportingpb.QueryTimeRange_PERIOD_6_HOURS
	case hours <= 24:
		return errorreportingpb.QueryTimeRange_PERIOD_1_DAY
	case hours <= 168:
		return errorreportingpb.QueryTimeRange_PERIOD_1_WEEK
	default:
		return errorreportingpb.QueryTimeRange_PERIOD_30_DAYS
	}
}

// ListErrors lists error groups sorted by occurrence count.
func ListErrors(ctx context.Context, client *errorreporting.ErrorStatsClient, project string, timeRangeHours, limit int, serviceFilter, versionFilter string) (*ErrorGroupList, error) {
	ctx, cancel := context.WithTimeout(ctx, errorReportingTimeout)
	defer cancel()

	req := &errorreportingpb.ListGroupStatsRequest{
		ProjectName: fmt.Sprintf("projects/%s", project),
		TimeRange: &errorreportingpb.QueryTimeRange{
			Period: timeRangePeriod(timeRangeHours),
		},
		PageSize: safeInt32(limit),
		Order:    errorreportingpb.ErrorGroupOrder_COUNT_DESC,
	}

	if serviceFilter != "" || versionFilter != "" {
		req.ServiceFilter = &errorreportingpb.ServiceContextFilter{
			Service: serviceFilter,
			Version: versionFilter,
		}
	}

	it := client.ListGroupStats(ctx, req)

	var groups []ErrorGroup
	for i := 0; i < limit; i++ {
		stats, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("iterating error groups: %w", err)
		}

		g := ErrorGroup{
			Count:     stats.Count,
			FirstSeen: formatTimestamp(stats.FirstSeenTime),
			LastSeen:  formatTimestamp(stats.LastSeenTime),
		}

		if stats.Group != nil {
			g.GroupID = stats.Group.GroupId
		}

		// Extract service and versions from affected services
		if len(stats.AffectedServices) > 0 {
			g.Service = stats.AffectedServices[0].Service
			versions := make(map[string]bool)
			for _, svc := range stats.AffectedServices {
				if svc.Version != "" {
					versions[svc.Version] = true
				}
			}
			for v := range versions {
				g.AffectedVersions = append(g.AffectedVersions, v)
			}
			sort.Strings(g.AffectedVersions)
		}

		// Extract message from representative event
		if stats.Representative != nil {
			g.Message = stats.Representative.Message
		}

		groups = append(groups, g)
	}

	return &ErrorGroupList{
		Count:  len(groups),
		Groups: groups,
	}, nil
}

// GetErrorGroup retrieves details for a specific error group.
func GetErrorGroup(ctx context.Context, client *errorreporting.ErrorStatsClient, project, groupID string, limit int) (*ErrorGroupDetail, error) {
	ctx, cancel := context.WithTimeout(ctx, errorReportingTimeout)
	defer cancel()

	req := &errorreportingpb.ListEventsRequest{
		ProjectName: fmt.Sprintf("projects/%s", project),
		GroupId:     groupID,
		PageSize:    safeInt32(limit),
	}

	it := client.ListEvents(ctx, req)

	detail := &ErrorGroupDetail{
		GroupID: groupID,
	}

	var instances []ErrorInstance
	for i := 0; i < limit; i++ {
		event, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("iterating error events: %w", err)
		}

		inst := ErrorInstance{
			Timestamp: formatTimestamp(event.EventTime),
			Message:   event.Message,
		}

		if event.ServiceContext != nil {
			inst.Service = event.ServiceContext.Service
			inst.Version = event.ServiceContext.Version
		}

		// Set detail-level fields from first event
		if len(instances) == 0 {
			detail.Message = event.Message
			if event.ServiceContext != nil {
				detail.Service = event.ServiceContext.Service
			}
		}

		instances = append(instances, inst)
	}

	if len(instances) == 0 {
		return nil, fmt.Errorf("no events found for error group %q in the current time window: the group may have been resolved or events may have aged out of retention", groupID)
	}

	detail.Instances = instances
	return detail, nil
}
