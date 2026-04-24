package internal

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateRegistryCommand(t *testing.T) {
	t.Run("valid overlay prints OK and exits zero", func(t *testing.T) {
		yaml := `metrics:
  "compute.googleapis.com/instance/cpu/utilization":
    slo_threshold: 0.50
`
		path := writeTempYAML(t, yaml)

		var out, errOut bytes.Buffer
		app := New(strings.NewReader(""), &out, &errOut)
		require.NoError(t, app.Run(context.Background(), []string{"mcp-gcp-observability", "validate-registry", path}))
		assert.Contains(t, out.String(), "OK:")
	})

	t.Run("no argument returns error", func(t *testing.T) {
		var out, errOut bytes.Buffer
		app := New(strings.NewReader(""), &out, &errOut)
		require.Error(t, app.Run(context.Background(), []string{"mcp-gcp-observability", "validate-registry"}))
	})

	t.Run("invalid registry returns error", func(t *testing.T) {
		// New metric missing required 'kind' field → LoadRegistry must reject it.
		yaml := `metrics:
  "custom.googleapis.com/ghost":
    slo_threshold: 0.5
`
		path := writeTempYAML(t, yaml)

		var out, errOut bytes.Buffer
		app := New(strings.NewReader(""), &out, &errOut)
		require.Error(t, app.Run(context.Background(), []string{"mcp-gcp-observability", "validate-registry", path}))
	})
}

// TestRunCommand_UnknownVariant verifies that --variant with an invalid value
// returns an error at the CLI layer without attempting a GCP connection.
// The variant guard in server.Run fires before gcpclient.New, so no credentials needed.
func TestRunCommand_UnknownVariant(t *testing.T) {
	var out, errOut bytes.Buffer
	app := New(strings.NewReader(""), &out, &errOut)
	err := app.Run(context.Background(), []string{
		"mcp-gcp-observability", "run",
		"--gcp-default-project=test-project",
		"--variant=bogus",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bogus")
	assert.Contains(t, err.Error(), "must be one of")
}

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}
