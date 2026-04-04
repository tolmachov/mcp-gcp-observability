package tools

import (
	"testing"

	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

func TestClassificationSeverity(t *testing.T) {
	tests := []struct {
		class metrics.Classification
		want  int
	}{
		{metrics.ClassStable, 0},
		{metrics.ClassNoisy, 1},
		{metrics.ClassRecovery, 2},
		{metrics.ClassSpike, 3},
		{metrics.ClassStepRegression, 4},
		{metrics.ClassSustainedRegression, 5},
		{metrics.ClassSaturation, 6},
	}

	for _, tt := range tests {
		t.Run(string(tt.class), func(t *testing.T) {
			got := classificationSeverity(tt.class)
			if got != tt.want {
				t.Errorf("classificationSeverity(%q) = %d, want %d", tt.class, got, tt.want)
			}
		})
	}
}

func TestClassificationSeverityOrdering(t *testing.T) {
	// Severity must be strictly increasing.
	classes := []metrics.Classification{
		metrics.ClassStable,
		metrics.ClassNoisy,
		metrics.ClassRecovery,
		metrics.ClassSpike,
		metrics.ClassStepRegression,
		metrics.ClassSustainedRegression,
		metrics.ClassSaturation,
	}
	for i := 1; i < len(classes); i++ {
		prev := classificationSeverity(classes[i-1])
		curr := classificationSeverity(classes[i])
		if curr <= prev {
			t.Errorf("severity(%q)=%d should be > severity(%q)=%d", classes[i], curr, classes[i-1], prev)
		}
	}
}

func TestClassificationSeverityUnknownIsHigh(t *testing.T) {
	// Unknown classifications should be treated as high severity (fail-safe).
	got := classificationSeverity(metrics.Classification("some_future_classification"))
	if got < classificationSeverity(metrics.ClassStepRegression) {
		t.Errorf("unknown severity = %d, should be >= %d (step_regression)", got, classificationSeverity(metrics.ClassStepRegression))
	}
}
