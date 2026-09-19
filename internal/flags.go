package internal

import "github.com/urfave/cli/v3"

const (
	flagGCPDefaultProject = "gcp-default-project"
	flagDNSServer         = "dns-server"
	flagMetricsRegistry   = "metrics-registry"
	flagTransport         = "transport"
	flagHTTPAddr          = "http-addr"
	flagVariant           = "variant"

	flagAuthIssuerURL          = "auth-issuer-url"
	flagAuthGoogleClientID     = "auth-google-client-id"
	flagAuthGoogleClientSecret = "auth-google-client-secret" //nolint:gosec // flag name, not a credential
	flagAuthAllowedDomains     = "auth-allowed-domains"
	flagAuthStateProject       = "auth-state-project"
	flagAuthStateDatabase      = "auth-state-database"
	flagAuthTokenKey           = "auth-token-key" //nolint:gosec // flag name, not a credential
	flagAuthAllowedRedirects   = "auth-allowed-redirects"
	flagAuthGoogleScopes       = "auth-google-scopes"
	flagAuthSkipConsent        = "auth-skip-consent"
)

func gcpDefaultProjectFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    flagGCPDefaultProject,
		Usage:   "Pin the public API to one GCP project. When omitted, every project-scoped tool requires project_id",
		Sources: cli.EnvVars("GCP_DEFAULT_PROJECT"),
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

func authIssuerURLFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    flagAuthIssuerURL,
		Usage:   "Public HTTPS base URL of this service; required for the HTTP transport",
		Sources: cli.EnvVars("AUTH_ISSUER_URL"),
	}
}

func authGoogleClientIDFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    flagAuthGoogleClientID,
		Usage:   "Google OAuth web client ID used for mandatory HTTP authentication",
		Sources: cli.EnvVars("AUTH_GOOGLE_CLIENT_ID"),
	}
}

func authGoogleClientSecretFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    flagAuthGoogleClientSecret,
		Usage:   "Google OAuth web client secret for HTTP transport (pass via Secret Manager in production)",
		Sources: cli.EnvVars("AUTH_GOOGLE_CLIENT_SECRET"),
	}
}

func authAllowedDomainsFlag() *cli.StringSliceFlag {
	return &cli.StringSliceFlag{
		Name:    flagAuthAllowedDomains,
		Usage:   "Google Workspace domains allowed to log in (comma-separated); required for unpinned HTTP deployments",
		Sources: cli.EnvVars("AUTH_ALLOWED_DOMAINS"),
	}
}

func authStateProjectFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    flagAuthStateProject,
		Usage:   "GCP project containing the Firestore OAuth state database; required for HTTP",
		Sources: cli.EnvVars("AUTH_STATE_PROJECT"),
	}
}

func authStateDatabaseFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    flagAuthStateDatabase,
		Usage:   "Firestore database used for OAuth state",
		Sources: cli.EnvVars("AUTH_STATE_DATABASE"),
		Value:   "(default)",
	}
}

func authTokenKeyFlag() *cli.StringSliceFlag {
	return &cli.StringSliceFlag{
		Name:    flagAuthTokenKey,
		Usage:   "Base64-encoded 32-byte token encryption keys (comma-separated; first encrypts, all decrypt). Generate with: openssl rand -base64 32",
		Sources: cli.EnvVars("AUTH_TOKEN_KEY"),
	}
}

func authAllowedRedirectsFlag() *cli.StringSliceFlag {
	return &cli.StringSliceFlag{
		Name:    flagAuthAllowedRedirects,
		Usage:   "Additional exact-match OAuth redirect URIs to allow (comma-separated). Loopback URIs and the claude.ai/claude.com callbacks are always allowed.",
		Sources: cli.EnvVars("AUTH_ALLOWED_REDIRECTS"),
	}
}

func authGoogleScopesFlag() *cli.StringSliceFlag {
	return &cli.StringSliceFlag{
		Name:    flagAuthGoogleScopes,
		Usage:   "Google OAuth scopes requested at login (comma-separated). Default: openid, email, cloud-platform (Error Reporting and Profiler accept no narrower scope; IAM is the authorization boundary)",
		Sources: cli.EnvVars("AUTH_GOOGLE_SCOPES"),
	}
}

func authSkipConsentFlag() *cli.BoolFlag {
	return &cli.BoolFlag{
		Name:    flagAuthSkipConsent,
		Usage:   "Skip the consent page and redirect straight to Google (development only)",
		Sources: cli.EnvVars("AUTH_SKIP_CONSENT"),
	}
}
