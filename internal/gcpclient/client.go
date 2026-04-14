package gcpclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/api/option"
	"google.golang.org/grpc"

	errorreporting "cloud.google.com/go/errorreporting/apiv1beta1"
	logging "cloud.google.com/go/logging/apiv2"
	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	cloudprofiler "cloud.google.com/go/cloudprofiler/apiv2"
	cloudtrace "cloud.google.com/go/trace/apiv1"
)

// Client wraps GCP API clients for Logging, Error Reporting, Cloud Trace, Cloud Monitoring, and Cloud Profiler.
type Client struct {
	logging    *logging.Client
	errors     *errorreporting.ErrorStatsClient
	trace      *cloudtrace.Client
	monitoring *monitoring.MetricClient
	profiler   *cloudprofiler.ExportClient
	config     *Config
	closeOnce  sync.Once
	closeErr   error
}

// LoggingClient returns the Cloud Logging API client.
func (c *Client) LoggingClient() *logging.Client { return c.logging }

// ErrorsClient returns the Error Reporting API client.
func (c *Client) ErrorsClient() *errorreporting.ErrorStatsClient { return c.errors }

// TraceClient returns the Cloud Trace API client.
func (c *Client) TraceClient() *cloudtrace.Client { return c.trace }

// MonitoringClient returns the Cloud Monitoring API client.
func (c *Client) MonitoringClient() *monitoring.MetricClient { return c.monitoring }

// ProfilerClient returns the Cloud Profiler gRPC export client.
func (c *Client) ProfilerService() *cloudprofiler.ExportClient { return c.profiler }

// Config returns a copy of the client configuration.
func (c *Client) Config() Config { return *c.config }

// New creates a new GCP client with Logging, Error Reporting, Cloud Trace, Cloud Monitoring,
// and Cloud Profiler API clients. Optionally configures a custom DNS resolver from Config.DNSServer.
func New(ctx context.Context, cfg *Config) (*Client, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config must not be nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	opts := clientOptions(cfg)

	loggingClient, err := logging.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating logging client: %w", err)
	}

	errorsClient, err := errorreporting.NewErrorStatsClient(ctx, opts...)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("creating error stats client: %w", err), loggingClient.Close())
	}

	traceClient, err := cloudtrace.NewClient(ctx, opts...)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("creating trace client: %w", err), loggingClient.Close(), errorsClient.Close())
	}

	monitoringClient, err := monitoring.NewMetricClient(ctx, opts...)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("creating monitoring client: %w", err), loggingClient.Close(), errorsClient.Close(), traceClient.Close())
	}

	profilerClient, err := cloudprofiler.NewExportClient(ctx, opts...)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("creating profiler client: %w", err), loggingClient.Close(), errorsClient.Close(), traceClient.Close(), monitoringClient.Close())
	}

	cfgCopy := *cfg
	return &Client{
		logging:    loggingClient,
		errors:     errorsClient,
		trace:      traceClient,
		monitoring: monitoringClient,
		profiler:   profilerClient,
		config:     &cfgCopy,
	}, nil
}

// Close closes all GCP API clients. It is safe to call multiple times.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		var errs []error
		if c.logging != nil {
			errs = append(errs, c.logging.Close())
		}
		if c.errors != nil {
			errs = append(errs, c.errors.Close())
		}
		if c.trace != nil {
			errs = append(errs, c.trace.Close())
		}
		if c.monitoring != nil {
			errs = append(errs, c.monitoring.Close())
		}
		if c.profiler != nil {
			errs = append(errs, c.profiler.Close())
		}
		c.closeErr = errors.Join(errs...)
	})
	return c.closeErr
}

// clientOptions returns Google API client options that configure a custom DNS resolver, or nil if no custom DNS server is configured.
func clientOptions(cfg *Config) []option.ClientOption {
	if cfg.DNSServer == "" {
		return nil
	}

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "udp", cfg.DNSServer+":53")
		},
	}

	dialer := &net.Dialer{
		Timeout:  10 * time.Second,
		Resolver: resolver,
	}

	return []option.ClientOption{
		option.WithGRPCDialOption(grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", addr)
		})),
	}
}
