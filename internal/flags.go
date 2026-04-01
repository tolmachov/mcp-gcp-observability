package internal

import "github.com/urfave/cli/v3"

const (
	flagGCPDefaultProject = "gcp-default-project"
	flagLogsMaxLimit      = "logs-max-limit"
	flagErrorsMaxLimit    = "errors-max-limit"
	flagDNSServer         = "dns-server"
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
