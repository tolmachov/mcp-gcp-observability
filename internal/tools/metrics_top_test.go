package tools

import "testing"

func TestSplitDimension(t *testing.T) {
	tests := []struct {
		input      string
		wantPrefix string
		wantKey    string
	}{
		{"metric.labels.response_code", "metric", "response_code"},
		{"resource.labels.instance_id", "resource", "instance_id"},
		{"response_code", "", "response_code"},
		{"metric.labels.", "", "metric.labels."},  // malformed: treated as bare key
		{"resource.labels.zone", "resource", "zone"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			p := splitDimension(tt.input)
			if p.prefix != tt.wantPrefix {
				t.Errorf("prefix = %q, want %q", p.prefix, tt.wantPrefix)
			}
			if p.key != tt.wantKey {
				t.Errorf("key = %q, want %q", p.key, tt.wantKey)
			}
		})
	}
}
