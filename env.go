package main

import "os"

// insecureSkipVerifyEnv exposes the TLS verification switch.
// Set SITEPROXY_INSECURE_SKIP_VERIFY=1 only for debugging MITM setups.
func insecureSkipVerifyEnv() string { return os.Getenv("SITEPROXY_INSECURE_SKIP_VERIFY") }
