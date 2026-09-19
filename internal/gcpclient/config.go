package gcpclient

// Config holds configuration for GCP API clients.
type Config struct {
	DefaultProject      string
	DNSServer           string
	MetricsRegistryFile string
}

// Validate checks the client configuration. DefaultProject is deliberately
// optional: the public API's ProjectPolicy decides whether a deployment is
// pinned or requires an explicit project on every request.
func (c *Config) Validate() error {
	return nil
}
