package gcpclient

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigValidateAllowsPinnedAndUnpinned(t *testing.T) {
	require.NoError(t, (&Config{DefaultProject: "my-project"}).Validate())
	require.NoError(t, (&Config{}).Validate())
}
