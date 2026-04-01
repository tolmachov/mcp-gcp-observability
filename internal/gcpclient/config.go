package gcpclient

// Config holds configuration for GCP API clients.
type Config struct {
	DefaultProject string
	LogsMaxLimit   int
	ErrorsMaxLimit int
	DNSServer      string
}
