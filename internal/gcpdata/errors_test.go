package gcpdata

import (
	"testing"

	"cloud.google.com/go/errorreporting/apiv1beta1/errorreportingpb"
)

func TestTimeRangePeriod(t *testing.T) {
	tests := []struct {
		name  string
		hours int
		want  errorreportingpb.QueryTimeRange_Period
	}{
		{"1 hour", 1, errorreportingpb.QueryTimeRange_PERIOD_1_HOUR},
		{"0 hours", 0, errorreportingpb.QueryTimeRange_PERIOD_1_HOUR},
		{"2 hours rounds to 6h", 2, errorreportingpb.QueryTimeRange_PERIOD_6_HOURS},
		{"6 hours exact", 6, errorreportingpb.QueryTimeRange_PERIOD_6_HOURS},
		{"7 hours rounds to 1d", 7, errorreportingpb.QueryTimeRange_PERIOD_1_DAY},
		{"24 hours exact", 24, errorreportingpb.QueryTimeRange_PERIOD_1_DAY},
		{"25 hours rounds to 1w", 25, errorreportingpb.QueryTimeRange_PERIOD_1_WEEK},
		{"168 hours (1 week) exact", 168, errorreportingpb.QueryTimeRange_PERIOD_1_WEEK},
		{"169 hours rounds to 30d", 169, errorreportingpb.QueryTimeRange_PERIOD_30_DAYS},
		{"720 hours (30 days)", 720, errorreportingpb.QueryTimeRange_PERIOD_30_DAYS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := timeRangePeriod(tt.hours)
			if got != tt.want {
				t.Errorf("timeRangePeriod(%d) = %v, want %v", tt.hours, got, tt.want)
			}
		})
	}
}
