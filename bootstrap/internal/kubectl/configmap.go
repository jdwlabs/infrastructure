package kubectl

import (
	"context"
	"fmt"
	"time"
)

// GetConfigMapData returns a ConfigMap's data map as raw JSON. Read-only, and
// bounded like the other inspection calls so a stalled API server cannot hang
// a caller indefinitely.
func (c *Client) GetConfigMapData(ctx context.Context, namespace, name string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmdArgs := []string{"-n", namespace, "get", "cm", name, "-o", "jsonpath={.data}"}
	ac, cmd := c.auditedCommand(ctx, cmdArgs...)
	output, err := combinedOutput(ac, cmd)
	if err != nil {
		return nil, fmt.Errorf("%s: %w, output: %s", c.cmdString(cmdArgs...), err, string(output))
	}
	return output, nil
}
