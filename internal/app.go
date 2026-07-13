package internal

import (
	"context"
	"fmt"
	"io"

	"github.com/urfave/cli/v3"

	"github.com/tolmachov/mcp-gcp-observability/internal/authsrv"
	"github.com/tolmachov/mcp-gcp-observability/internal/gcpclient"
	"github.com/tolmachov/mcp-gcp-observability/internal/metrics"
	"github.com/tolmachov/mcp-gcp-observability/internal/server"
)

// Version contains semantic version number of application.
var Version = "dev"

const serviceName = "mcp-gcp-observability"

// authConfigFromFlags builds the auth configuration from CLI flags, or nil
// when --auth is 'none'. Validation of the individual fields happens in
// authsrv.Config.Validate at server startup.
func authConfigFromFlags(cmd *cli.Command) (*authsrv.Config, error) {
	switch authsrv.Mode(cmd.String(flagAuth)) {
	case authsrv.ModeNone, "":
		return nil, nil
	case authsrv.ModeGoogle:
		return &authsrv.Config{
			IssuerURL:            cmd.String(flagAuthIssuerURL),
			GoogleClientID:       cmd.String(flagAuthGoogleClientID),
			GoogleClientSecret:   cmd.String(flagAuthGoogleClientSecret),
			AllowedDomains:       cmd.StringSlice(flagAuthAllowedDomains),
			RequireProjectAccess: cmd.String(flagAuthRequireProject),
			TokenKeys:            cmd.StringSlice(flagAuthTokenKey),
			ExtraRedirects:       cmd.StringSlice(flagAuthAllowedRedirects),
			Scopes:               cmd.StringSlice(flagAuthGoogleScopes),
			SkipConsent:          cmd.Bool(flagAuthSkipConsent),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported auth mode %q: must be %q or %q",
			cmd.String(flagAuth), authsrv.ModeNone, authsrv.ModeGoogle)
	}
}

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
					authFlag(),
					authIssuerURLFlag(),
					authGoogleClientIDFlag(),
					authGoogleClientSecretFlag(),
					authAllowedDomainsFlag(),
					authRequireProjectFlag(),
					authTokenKeyFlag(),
					authAllowedRedirectsFlag(),
					authGoogleScopesFlag(),
					authSkipConsentFlag(),
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					cfg := &gcpclient.Config{
						DefaultProject:      cmd.String(flagGCPDefaultProject),
						LogsMaxLimit:        cmd.Int(flagLogsMaxLimit),
						ErrorsMaxLimit:      cmd.Int(flagErrorsMaxLimit),
						DNSServer:           cmd.String(flagDNSServer),
						MetricsRegistryFile: cmd.String(flagMetricsRegistry),
					}
					authCfg, err := authConfigFromFlags(cmd)
					if err != nil {
						return err
					}
					switch {
					case authCfg == nil && cfg.DefaultProject == "":
						return fmt.Errorf("GCP_DEFAULT_PROJECT is required (it is optional only with --auth google, where each user chooses a project at login)")
					case authCfg != nil && cfg.DefaultProject == "":
						// No pinned project: users pick one on the consent
						// page; access to the choice is verified at login.
						authCfg.AllowProjectChoice = true
					case authCfg != nil && authCfg.RequireProjectAccess == "":
						// Pinned project: the server works only with it, so
						// logging in requires IAM access to it.
						authCfg.RequireProjectAccess = cfg.DefaultProject
					}
					srv, err := server.New(cfg, Version, cmd.Root().Reader, cmd.Root().Writer, cmd.Root().ErrWriter)
					if err != nil {
						return err
					}
					return srv.Run(ctx, server.RunOptions{
						Transport: server.Transport(cmd.String(flagTransport)),
						HTTPAddr:  cmd.String(flagHTTPAddr),
						VariantID: cmd.String(flagVariant),
						Auth:      authCfg,
					})
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
