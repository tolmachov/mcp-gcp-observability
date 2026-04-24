package internal

import (
	"context"
	"fmt"
	"io"

	"github.com/urfave/cli/v3"

	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
	"github.com/tolmachov/mcp-gcp-observability/internal/server"
)

// Version contains semantic version number of application.
var Version = "dev"

const serviceName = "mcp-gcp-observability"

// New creates a new CLI application.
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
					variantFlag(),
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
					variantID := cmd.String(flagVariant)
					return srv.Run(ctx, transport, httpAddr, variantID)
				},
			},
			{
				Name:      "validate-registry",
				Usage:     "Validate a metrics registry overlay YAML against the embedded schema",
				ArgsUsage: "<path-to-registry.yaml>",
				Description: "Loads the given YAML file as a user overlay on top of the embedded default " +
					"registry and reports any parse, merge, or validation errors. Exits non-zero if the " +
					"file would be rejected by the server at startup. Use this after generating a custom " +
					"registry (e.g. via the generate-metrics-registry MCP prompt) to catch mistakes before " +
					"wiring the file up with METRICS_REGISTRY_FILE.",
				Action: func(_ context.Context, cmd *cli.Command) error {
					if cmd.NArg() != 1 {
						return fmt.Errorf("validate-registry requires exactly one argument: the path to the registry YAML file")
					}
					path := cmd.Args().Get(0)
					reg, err := metrics.LoadRegistry(path)
					if err != nil {
						return fmt.Errorf("registry %q is invalid: %w", path, err)
					}
					out := cmd.Root().Writer
					if _, err := fmt.Fprintf(out, "OK: %s loaded successfully (%d metrics total after merge with embedded defaults)\n", path, reg.Count()); err != nil {
						return fmt.Errorf("writing output: %w", err)
					}
					return nil
				},
			},
		},
	}
}
