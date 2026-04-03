package gcpclient

import "fmt"

// Config holds configuration for GCP API clients.
type Config struct {
	DefaultProject string
	LogsMaxLimit   int
	ErrorsMaxLimit int
	DNSServer      string
}

// Validate checks that DefaultProject is non-empty and that LogsMaxLimit and ErrorsMaxLimit are positive.
func (c *Config) Validate() error {
	if c.DefaultProject == "" {
		return fmt.Errorf("default project is required: set GCP_DEFAULT_PROJECT environment variable")
	}
	if c.LogsMaxLimit <= 0 {
		return fmt.Errorf("logs max limit must be positive, got %d", c.LogsMaxLimit)
	}
	if c.ErrorsMaxLimit <= 0 {
		return fmt.Errorf("errors max limit must be positive, got %d", c.ErrorsMaxLimit)
	}
	return nil
}
