package tools

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpdata"
)

func TestLimitFlamegraphNodesReportsExactOmission(t *testing.T) {
	root := &gcpdata.FlamegraphNode{Name: "root"}
	for i := range 1_099 {
		root.Children = append(root.Children, gcpdata.FlamegraphNode{Name: fmt.Sprintf("n-%d", i)})
	}
	omitted := limitFlamegraphNodes(root, 1_000)
	assert.Equal(t, 100, omitted)
	assert.Equal(t, 1_000, flamegraphNodeCount(root))
}
