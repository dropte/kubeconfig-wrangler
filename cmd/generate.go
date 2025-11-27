package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/rancher-kubeconfig-proxy/pkg/config"
	"github.com/rancher-kubeconfig-proxy/pkg/kubeconfig"
	"github.com/rancher-kubeconfig-proxy/pkg/rancher"
)

var (
	rancherURL      string
	accessKey       string
	secretKey       string
	token           string
	username        string
	password        string
	clusterPrefix   string
	outputPath      string
	insecureSkipTLS bool
	caCert          string
)

// generateCmd represents the generate command
var generateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate a kubeconfig file from Rancher clusters",
	Long: `Generate a merged kubeconfig file containing all active downstream
Kubernetes clusters managed by the specified Rancher instance.

The kubeconfig file can be used with kubectl, helm, and other Kubernetes tools.

Authentication can be provided via:
  - API token (--token or RANCHER_TOKEN)
  - Access key/secret key pair (--access-key/--secret-key or RANCHER_ACCESS_KEY/RANCHER_SECRET_KEY)
  - Username/password (--username/--password or RANCHER_USERNAME/RANCHER_PASSWORD)

Examples:
  # Generate kubeconfig using API token
  rancher-kubeconfig-proxy generate --url https://rancher.example.com --token token-xxxxx:yyyyyyy

  # Generate kubeconfig using username/password
  rancher-kubeconfig-proxy generate --url https://rancher.example.com --username admin --password mypassword

  # Generate kubeconfig with cluster name prefix
  rancher-kubeconfig-proxy generate --url https://rancher.example.com --token token-xxxxx:yyyyyyy --prefix "prod-"

  # Generate kubeconfig to a specific file
  rancher-kubeconfig-proxy generate --url https://rancher.example.com --username admin --password mypassword --output ~/.kube/rancher-config

  # Using environment variables
  export RANCHER_URL=https://rancher.example.com
  export RANCHER_USERNAME=admin
  export RANCHER_PASSWORD=mypassword
  export RANCHER_CLUSTER_PREFIX=prod-
  rancher-kubeconfig-proxy generate`,
	RunE: runGenerate,
}

func init() {
	generateCmd.Flags().StringVarP(&rancherURL, "url", "u", "", "Rancher server URL (env: RANCHER_URL)")
	generateCmd.Flags().StringVarP(&accessKey, "access-key", "a", "", "Rancher API access key (env: RANCHER_ACCESS_KEY)")
	generateCmd.Flags().StringVarP(&secretKey, "secret-key", "s", "", "Rancher API secret key (env: RANCHER_SECRET_KEY)")
	generateCmd.Flags().StringVarP(&token, "token", "t", "", "Rancher API token (access_key:secret_key) (env: RANCHER_TOKEN)")
	generateCmd.Flags().StringVar(&username, "username", "", "Rancher username for password auth (env: RANCHER_USERNAME)")
	generateCmd.Flags().StringVar(&password, "password", "", "Rancher password for password auth (env: RANCHER_PASSWORD)")
	generateCmd.Flags().StringVarP(&clusterPrefix, "prefix", "p", "", "Prefix to add to cluster names (env: RANCHER_CLUSTER_PREFIX)")
	generateCmd.Flags().StringVarP(&outputPath, "output", "o", "", "Output file path (default: stdout) (env: RANCHER_KUBECONFIG_OUTPUT)")
	generateCmd.Flags().BoolVarP(&insecureSkipTLS, "insecure-skip-tls-verify", "k", false, "Skip TLS certificate verification (env: RANCHER_INSECURE_SKIP_TLS_VERIFY)")
	generateCmd.Flags().StringVar(&caCert, "ca-cert", "", "Path to CA certificate file (env: RANCHER_CA_CERT)")
}

func runGenerate(cmd *cobra.Command, args []string) error {
	// Build configuration from flags and environment
	cfg := config.LoadFromEnv()

	// Override with command line flags if provided
	if rancherURL != "" {
		cfg.RancherURL = rancherURL
	}
	if accessKey != "" {
		cfg.AccessKey = accessKey
	}
	if secretKey != "" {
		cfg.SecretKey = secretKey
	}
	if token != "" {
		cfg.Token = token
	}
	if username != "" {
		cfg.Username = username
	}
	if password != "" {
		cfg.Password = password
	}
	if clusterPrefix != "" {
		cfg.ClusterPrefix = clusterPrefix
	}
	if outputPath != "" {
		cfg.OutputPath = outputPath
	}
	if cmd.Flags().Changed("insecure-skip-tls-verify") {
		cfg.InsecureSkipTLSVerify = insecureSkipTLS
	}
	if caCert != "" {
		cfg.CACert = caCert
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("configuration error: %w", err)
	}

	// Create Rancher client
	client, err := rancher.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("failed to create Rancher client: %w", err)
	}

	// Get kubeconfigs for all clusters
	fmt.Fprintln(os.Stderr, "Fetching clusters from Rancher...")
	kubeconfigs, err := client.GetAllKubeconfigs()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfigs: %w", err)
	}

	if len(kubeconfigs) == 0 {
		return fmt.Errorf("no active clusters found")
	}

	fmt.Fprintf(os.Stderr, "Found %d active cluster(s)\n", len(kubeconfigs))

	// Generate merged kubeconfig
	generator := kubeconfig.NewGenerator(cfg.ClusterPrefix)
	kubeconfigData, err := generator.Generate(kubeconfigs)
	if err != nil {
		return fmt.Errorf("failed to generate kubeconfig: %w", err)
	}

	// Output the kubeconfig
	if cfg.OutputPath != "" {
		if err := os.WriteFile(cfg.OutputPath, kubeconfigData, 0600); err != nil {
			return fmt.Errorf("failed to write kubeconfig to %s: %w", cfg.OutputPath, err)
		}
		fmt.Fprintf(os.Stderr, "Kubeconfig written to %s\n", cfg.OutputPath)
	} else {
		fmt.Print(string(kubeconfigData))
	}

	return nil
}
