// Package config provides configuration handling for the rancher-kubeconfig-proxy
package config

import (
	"errors"
	"os"
	"strings"
)

// Config holds the application configuration
type Config struct {
	// RancherURL is the URL of the Rancher server (e.g., https://rancher.example.com)
	RancherURL string

	// AccessKey is the Rancher API access key (username part of the token)
	AccessKey string

	// SecretKey is the Rancher API secret key (password part of the token)
	SecretKey string

	// Token is the combined access_key:secret_key token (alternative to AccessKey/SecretKey)
	Token string

	// ClusterPrefix is the prefix to add to cluster names in the kubeconfig
	ClusterPrefix string

	// OutputPath is the path where the kubeconfig file will be written (empty for stdout)
	OutputPath string

	// InsecureSkipTLSVerify skips TLS certificate verification
	InsecureSkipTLSVerify bool

	// CACert is the path to a CA certificate file for TLS verification
	CACert string
}

// Validate checks if the configuration is valid
func (c *Config) Validate() error {
	if c.RancherURL == "" {
		return errors.New("rancher URL is required")
	}

	// Ensure URL doesn't have trailing slash
	c.RancherURL = strings.TrimSuffix(c.RancherURL, "/")

	// Check authentication - either token or access_key/secret_key pair is required
	if c.Token == "" && (c.AccessKey == "" || c.SecretKey == "") {
		return errors.New("either token or access_key/secret_key pair is required")
	}

	// If token is provided, split it into access_key and secret_key
	if c.Token != "" {
		parts := strings.SplitN(c.Token, ":", 2)
		if len(parts) != 2 {
			return errors.New("invalid token format, expected 'access_key:secret_key'")
		}
		c.AccessKey = parts[0]
		c.SecretKey = parts[1]
	}

	return nil
}

// LoadFromEnv loads configuration from environment variables
func LoadFromEnv() *Config {
	return &Config{
		RancherURL:            os.Getenv("RANCHER_URL"),
		AccessKey:             os.Getenv("RANCHER_ACCESS_KEY"),
		SecretKey:             os.Getenv("RANCHER_SECRET_KEY"),
		Token:                 os.Getenv("RANCHER_TOKEN"),
		ClusterPrefix:         os.Getenv("RANCHER_CLUSTER_PREFIX"),
		OutputPath:            os.Getenv("RANCHER_KUBECONFIG_OUTPUT"),
		InsecureSkipTLSVerify: os.Getenv("RANCHER_INSECURE_SKIP_TLS_VERIFY") == "true",
		CACert:                os.Getenv("RANCHER_CA_CERT"),
	}
}

// GetBasicAuth returns the basic auth credentials for the Rancher API
func (c *Config) GetBasicAuth() (username, password string) {
	return c.AccessKey, c.SecretKey
}
