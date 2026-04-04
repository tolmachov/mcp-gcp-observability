package internal

import (
	"context"
	"io"

	"github.com/urfave/cli/v3"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/server"
)

// Version contains semantic version number of application.
var Version = "dev"

const serviceName = "mcp-gcp-observability"

// New creates a new instance of application.
func New(in io.Reader, out, errOut io.Writer) *cli.Command {
	return &cli.Command{
		Name:      serviceName,
		Version:   Version,
		Usage:     "MCP server for GCP Cloud Logging, Error Reporting, and Cloud Trace",
		Reader:    in,
		Writer:    out,
		ErrWriter: errOut,
		Commands: []*cli.Command{
			{
				Name:  "run",
				Usage: "Run the MCP server",
				Flags: []cli.Flag{
					gcpDefaultProjectFlag(),
					logsMaxLimitFlag(),
					errorsMaxLimitFlag(),
					dnsServerFlag(),
					metricsRegistryFlag(),
					transportFlag(),
					httpAddrFlag(),
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					cfg := &gcpclient.Config{
						DefaultProject:      cmd.String(flagGCPDefaultProject),
						LogsMaxLimit:        cmd.Int(flagLogsMaxLimit),
						ErrorsMaxLimit:      cmd.Int(flagErrorsMaxLimit),
						DNSServer:           cmd.String(flagDNSServer),
						MetricsRegistryFile: cmd.String(flagMetricsRegistry),
					}
					srv, err := server.New(cfg, Version, cmd.Root().Reader, cmd.Root().Writer, cmd.Root().ErrWriter)
					if err != nil {
						return err
					}
					transport := server.Transport(cmd.String(flagTransport))
					httpAddr := cmd.String(flagHTTPAddr)
					return srv.Run(ctx, transport, httpAddr)
				},
			},
		},
	}
}
