// Package config provides configuration handling for the rancher-kubeconfig-proxy
package config

import (
	"errors"
	"os"
	"strings"
)

// AuthMethod represents the authentication method to use
type AuthMethod string

const (
	// AuthMethodToken uses API token authentication (access_key:secret_key)
	AuthMethodToken AuthMethod = "token"
	// AuthMethodPassword uses username/password authentication
	AuthMethodPassword AuthMethod = "password"
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

	// Username is the Rancher username for password authentication
	Username string

	// Password is the Rancher password for password authentication
	Password string

	// AuthMethod indicates which authentication method to use
	AuthMethod AuthMethod

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

	// Determine authentication method based on provided credentials
	hasToken := c.Token != "" || (c.AccessKey != "" && c.SecretKey != "")
	hasPassword := c.Username != "" && c.Password != ""

	if !hasToken && !hasPassword {
		return errors.New("authentication required: provide either token/access_key+secret_key or username+password")
	}

	// If both are provided, prefer token auth unless explicitly set to password
	if hasToken && hasPassword && c.AuthMethod == "" {
		c.AuthMethod = AuthMethodToken
	} else if hasPassword && !hasToken {
		c.AuthMethod = AuthMethodPassword
	} else if hasToken {
		c.AuthMethod = AuthMethodToken
	}

	// If using token auth, parse the token if needed
	if c.AuthMethod == AuthMethodToken {
		if c.Token != "" {
			parts := strings.SplitN(c.Token, ":", 2)
			if len(parts) != 2 {
				return errors.New("invalid token format, expected 'access_key:secret_key'")
			}
			c.AccessKey = parts[0]
			c.SecretKey = parts[1]
		}
		if c.AccessKey == "" || c.SecretKey == "" {
			return errors.New("token authentication requires access_key and secret_key")
		}
	}

	// If using password auth, validate credentials
	if c.AuthMethod == AuthMethodPassword {
		if c.Username == "" || c.Password == "" {
			return errors.New("password authentication requires username and password")
		}
	}

	return nil
}

// UsePasswordAuth returns true if password authentication should be used
func (c *Config) UsePasswordAuth() bool {
	return c.AuthMethod == AuthMethodPassword
}

// LoadFromEnv loads configuration from environment variables
func LoadFromEnv() *Config {
	return &Config{
		RancherURL:            os.Getenv("RANCHER_URL"),
		AccessKey:             os.Getenv("RANCHER_ACCESS_KEY"),
		SecretKey:             os.Getenv("RANCHER_SECRET_KEY"),
		Token:                 os.Getenv("RANCHER_TOKEN"),
		Username:              os.Getenv("RANCHER_USERNAME"),
		Password:              os.Getenv("RANCHER_PASSWORD"),
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
