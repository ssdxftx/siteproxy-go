package main

import (
	"os"
	"strings"
)

// insecureSkipVerifyEnv exposes the TLS verification switch.
// Set SITEPROXY_INSECURE_SKIP_VERIFY=1 only for debugging MITM setups.
func insecureSkipVerifyEnv() string { return os.Getenv("SITEPROXY_INSECURE_SKIP_VERIFY") }

// useEnvProxy reports whether outbound requests should use the ambient
// HTTP_PROXY/HTTPS_PROXY configuration. Upstream node fetch does not, so this
// is opt-in to avoid surprising routing changes in shared environments.
func useEnvProxy() bool {
	v := os.Getenv("SITEPROXY_USE_ENV_PROXY")
	return v == "1" || strings.EqualFold(v, "true")
}
