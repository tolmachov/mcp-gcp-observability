package internal

import "github.com/urfave/cli/v3"

const (
	flagGCPDefaultProject = "gcp-default-project"
	flagLogsMaxLimit      = "logs-max-limit"
	flagErrorsMaxLimit    = "errors-max-limit"
	flagDNSServer         = "dns-server"
	flagMetricsRegistry   = "metrics-registry"
	flagTransport         = "transport"
	flagHTTPAddr          = "http-addr"
	flagVariant           = "variant"
)

func gcpDefaultProjectFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:     flagGCPDefaultProject,
		Usage:    "Default GCP project ID",
		Sources:  cli.EnvVars("GCP_DEFAULT_PROJECT"),
		Required: true,
	}
}

func logsMaxLimitFlag() *cli.IntFlag {
	return &cli.IntFlag{
		Name:    flagLogsMaxLimit,
		Usage:   "Maximum number of log entries to return",
		Sources: cli.EnvVars("LOGS_MAX_LIMIT"),
		Value:   1000,
	}
}

func errorsMaxLimitFlag() *cli.IntFlag {
	return &cli.IntFlag{
		Name:    flagErrorsMaxLimit,
		Usage:   "Maximum number of error groups to return",
		Sources: cli.EnvVars("ERRORS_MAX_LIMIT"),
		Value:   100,
	}
}

func dnsServerFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    flagDNSServer,
		Usage:   "Custom DNS server for GCP API resolution (e.g. 8.8.8.8)",
		Sources: cli.EnvVars("DNS_SERVER"),
	}
}

func metricsRegistryFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    flagMetricsRegistry,
		Usage:   "Path to metrics semantic registry YAML file (optional)",
		Sources: cli.EnvVars("METRICS_REGISTRY_FILE"),
	}
}

func transportFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    flagTransport,
		Usage:   "Transport mode: 'stdio' (default) or 'http' (streamable HTTP)",
		Sources: cli.EnvVars("MCP_TRANSPORT"),
		Value:   "stdio",
	}
}

func httpAddrFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    flagHTTPAddr,
		Usage:   "HTTP listen address when transport is 'http' (default ':8080')",
		Sources: cli.EnvVars("MCP_HTTP_ADDR"),
		Value:   ":8080",
	}
}

func variantFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    flagVariant,
		Usage:   "Force a specific capability variant: 'full' (all tools, standard descriptions), 'compact' (all tools, short descriptions), or 'monitoring' (10 core tools). Omit to use the variants protocol and let clients negotiate.",
		Sources: cli.EnvVars("MCP_VARIANT"),
	}
}
