package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kubeconfig-wrangler/pkg/config"
)

// registerRancherFlags adds the Rancher connection and credential flags
// shared by the generate and list commands.
func registerRancherFlags(c *cobra.Command) {
	f := c.Flags()
	f.StringP("url", "u", "", "Rancher server URL (env: RANCHER_URL)")
	f.StringP("access-key", "a", "", "Rancher API access key (env: RANCHER_ACCESS_KEY)")
	f.StringP("secret-key", "s", "", "Rancher API secret key (env: RANCHER_SECRET_KEY)")
	f.StringP("token", "t", "", "Rancher API token (access_key:secret_key) (env: RANCHER_TOKEN)")
	f.String("username", "", "Rancher username for password auth, local accounts only (env: RANCHER_USERNAME)")
	f.String("password", "", "Rancher password for password auth, local accounts only (env: RANCHER_PASSWORD)")
	f.BoolP("insecure-skip-tls-verify", "k", false, "Skip TLS certificate verification (env: RANCHER_INSECURE_SKIP_TLS_VERIFY)")
	f.String("ca-cert", "", "Path to CA certificate file (env: RANCHER_CA_CERT)")
}

// rancherConfigFromFlags loads the Rancher configuration from the environment
// and applies the flags registered by registerRancherFlags on top of it.
//
// Credentials given explicitly as flags win over credentials of the other
// kind that came from the environment: --username/--password discard an
// env token or key pair, and --token/--access-key/--secret-key discard env
// username/password. Explicit key-pair flags also discard an env token.
// Any flag given explicitly, even as an empty string, overrides the
// environment value.
func rancherConfigFromFlags(c *cobra.Command) *config.Config {
	cfg := config.LoadFromEnv()
	f := c.Flags()
	changed := f.Changed
	str := func(name string) string {
		v, _ := f.GetString(name)
		return v
	}

	passwordFlags := changed("username") || changed("password")
	keyPairFlags := changed("access-key") || changed("secret-key")
	tokenFlag := changed("token")

	if passwordFlags && !keyPairFlags && !tokenFlag {
		cfg.Token, cfg.AccessKey, cfg.SecretKey = "", "", ""
	}
	if (keyPairFlags || tokenFlag) && !passwordFlags {
		cfg.Username, cfg.Password = "", ""
	}
	if keyPairFlags && !tokenFlag {
		cfg.Token = ""
	}

	for name, dst := range map[string]*string{
		"url":        &cfg.RancherURL,
		"access-key": &cfg.AccessKey,
		"secret-key": &cfg.SecretKey,
		"token":      &cfg.Token,
		"username":   &cfg.Username,
		"password":   &cfg.Password,
		"ca-cert":    &cfg.CACert,
	} {
		// Changed, not non-empty, so an explicit empty flag (e.g.
		// --ca-cert "") clears a value that came from the environment.
		if changed(name) {
			*dst = str(name)
		}
	}
	if changed("insecure-skip-tls-verify") {
		cfg.InsecureSkipTLSVerify, _ = f.GetBool("insecure-skip-tls-verify")
	}
	return cfg
}

// writeOutputFile writes a kubeconfig to path and makes sure the file is
// private even when it already existed with wider permissions.
func writeOutputFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

// countErrors returns how many errors err holds (errors.Join aware).
func countErrors(err error) int {
	if err == nil {
		return 0
	}
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		return len(joined.Unwrap())
	}
	return 1
}

// reportClusterFailures prints a summary of failed cluster fetches to stderr
// and returns the number of failures.
func reportClusterFailures(err error, succeeded int) int {
	failed := countErrors(err)
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "Warning: %d of %d clusters failed:\n%v\n", failed, failed+succeeded, err)
	}
	return failed
}

// partialError is returned after a partial kubeconfig was written.
func partialError(failed, succeeded int) error {
	return fmt.Errorf("%d of %d clusters failed; the generated kubeconfig is incomplete", failed, failed+succeeded)
}
