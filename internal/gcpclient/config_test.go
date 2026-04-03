package gcpclient

import (
	"testing"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{
			"valid",
			Config{DefaultProject: "my-project", LogsMaxLimit: 1000, ErrorsMaxLimit: 100},
			false,
		},
		{
			"minimal valid",
			Config{DefaultProject: "p", LogsMaxLimit: 1, ErrorsMaxLimit: 1},
			false,
		},
		{
			"empty project",
			Config{DefaultProject: "", LogsMaxLimit: 1000, ErrorsMaxLimit: 100},
			true,
		},
		{
			"zero logs limit",
			Config{DefaultProject: "p", LogsMaxLimit: 0, ErrorsMaxLimit: 100},
			true,
		},
		{
			"negative logs limit",
			Config{DefaultProject: "p", LogsMaxLimit: -1, ErrorsMaxLimit: 100},
			true,
		},
		{
			"zero errors limit",
			Config{DefaultProject: "p", LogsMaxLimit: 100, ErrorsMaxLimit: 0},
			true,
		},
		{
			"negative errors limit",
			Config{DefaultProject: "p", LogsMaxLimit: 100, ErrorsMaxLimit: -5},
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
