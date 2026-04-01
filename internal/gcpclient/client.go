package gcpclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"google.golang.org/api/option"
	"google.golang.org/grpc"

	errorreporting "cloud.google.com/go/errorreporting/apiv1beta1"
	logging "cloud.google.com/go/logging/apiv2"
)

// Client wraps GCP API clients for Logging and Error Reporting.
type Client struct {
	Logging *logging.Client
	Errors  *errorreporting.ErrorStatsClient
	Config  *Config
}

// New creates a new GCP client using Application Default Credentials.
func New(ctx context.Context, cfg *Config) (*Client, error) {
	opts := clientOptions(cfg)

	loggingClient, err := logging.NewClient(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating logging client: %w", err)
	}

	errorsClient, err := errorreporting.NewErrorStatsClient(ctx, opts...)
	if err != nil {
		_ = loggingClient.Close()
		return nil, fmt.Errorf("creating error stats client: %w", err)
	}

	return &Client{
		Logging: loggingClient,
		Errors:  errorsClient,
		Config:  cfg,
	}, nil
}

// Close closes all GCP API clients.
func (c *Client) Close() error {
	return errors.Join(
		c.Logging.Close(),
		c.Errors.Close(),
	)
}

// clientOptions builds Google API client options from config.
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
