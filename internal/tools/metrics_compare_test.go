package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
)

func TestClassificationSeverity(t *testing.T) {
	tests := []struct {
		class metrics.Classification
		want  int
	}{
		{metrics.ClassImprovement, -1},
		{metrics.ClassInsufficientData, 0},
		{metrics.ClassStable, 0},
		{metrics.ClassNoisy, 1},
		{metrics.ClassRecovery, 2},
		{metrics.ClassSpike, 3},
		{metrics.ClassFlapping, 4},
		{metrics.ClassStepRegression, 5},
		{metrics.ClassSustainedRegression, 6},
		{metrics.ClassSaturation, 7},
	}

	for _, tt := range tests {
		t.Run(string(tt.class), func(t *testing.T) {
			got := classificationSeverity(tt.class)
			assert.Equal(t, tt.want, got, "classificationSeverity(%v)", tt.class)
		})
	}
}

func TestClassificationSeverityOrdering(t *testing.T) {
	// Severity must be non-decreasing and end in saturation as the most
	// severe. insufficient_data deliberately shares rank with stable because
	// "we don't know" is not an alert.
	classes := []metrics.Classification{
		metrics.ClassImprovement,
		metrics.ClassStable,
		metrics.ClassNoisy,
		metrics.ClassRecovery,
		metrics.ClassSpike,
		metrics.ClassFlapping,
		metrics.ClassStepRegression,
		metrics.ClassSustainedRegression,
		metrics.ClassSaturation,
	}
	for i := 1; i < len(classes); i++ {
		prev := classificationSeverity(classes[i-1])
		curr := classificationSeverity(classes[i])
		assert.Greater(t, curr, prev, "severity(%v)=%d should be > severity(%v)=%d", classes[i], curr, classes[i-1], prev)
	}
}

func TestClassificationSeverityUnknownIsHigh(t *testing.T) {
	// Unknown classifications should be treated as high severity (fail-safe).
	got := classificationSeverity(metrics.Classification("some_future_classification"))
	assert.GreaterOrEqual(t, got, classificationSeverity(metrics.ClassStepRegression), "unknown severity should be >= step_regression")
}
