package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/kubeconfig-wrangler/pkg/config"
	"github.com/kubeconfig-wrangler/pkg/kubeconfig"
	"github.com/kubeconfig-wrangler/pkg/rancher"
)

var (
	clusterPrefix string
	outputPath    string
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
  - Username/password, Rancher local accounts only (--username/--password or RANCHER_USERNAME/RANCHER_PASSWORD);
    users of AD, LDAP, SAML or OIDC should use an API token

Examples:
  # Generate kubeconfig using API token
  kubeconfig-wrangler generate --url https://rancher.example.com --token token-xxxxx:yyyyyyy

  # Generate kubeconfig using username/password
  kubeconfig-wrangler generate --url https://rancher.example.com --username admin --password mypassword

  # Generate kubeconfig with cluster name prefix
  kubeconfig-wrangler generate --url https://rancher.example.com --token token-xxxxx:yyyyyyy --prefix "prod-"

  # Generate kubeconfig to a specific file
  kubeconfig-wrangler generate --url https://rancher.example.com --username admin --password mypassword --output ~/.kube/rancher-config

  # Using environment variables
  export RANCHER_URL=https://rancher.example.com
  export RANCHER_USERNAME=admin
  export RANCHER_PASSWORD=mypassword
  export RANCHER_CLUSTER_PREFIX=prod-
  kubeconfig-wrangler generate`,
	RunE: runGenerate,
}

func init() {
	registerRancherFlags(generateCmd)
	generateCmd.Flags().StringVarP(&clusterPrefix, "prefix", "p", "", "Prefix to add to cluster names (env: RANCHER_CLUSTER_PREFIX)")
	generateCmd.Flags().StringVarP(&outputPath, "output", "o", "", "Output file path (default: stdout) (env: RANCHER_KUBECONFIG_OUTPUT)")
}

func runGenerate(cmd *cobra.Command, args []string) error {
	// Build configuration from environment and flags
	cfg := rancherConfigFromFlags(cmd)
	applyGenerateFlags(cmd, cfg)

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("configuration error: %w", err)
	}

	// Create Rancher client
	client, err := rancher.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("failed to create Rancher client: %w", err)
	}
	defer client.Close()

	// Get kubeconfigs for all clusters
	fmt.Fprintln(os.Stderr, "Fetching clusters from Rancher...")
	kubeconfigs, fetchErr := client.GetAllKubeconfigs()
	if kubeconfigs == nil && fetchErr != nil {
		return fmt.Errorf("failed to get kubeconfigs: %w", fetchErr)
	}
	failed := reportClusterFailures(fetchErr, len(kubeconfigs))

	if len(kubeconfigs) == 0 {
		if failed > 0 {
			return fmt.Errorf("failed to fetch a kubeconfig for any of the %d active cluster(s)", failed)
		}
		return fmt.Errorf("no active clusters found")
	}

	fmt.Fprintf(os.Stderr, "Fetched kubeconfigs for %d active cluster(s)\n", len(kubeconfigs))

	// Generate merged kubeconfig
	generator := kubeconfig.NewGenerator(cfg.ClusterPrefix)
	kubeconfigData, err := generator.Generate(kubeconfigs)
	if err != nil {
		return fmt.Errorf("failed to generate kubeconfig: %w", err)
	}

	// Output the kubeconfig
	if cfg.OutputPath != "" {
		if err := writeOutputFile(cfg.OutputPath, kubeconfigData); err != nil {
			return fmt.Errorf("failed to write kubeconfig to %s: %w", cfg.OutputPath, err)
		}
		fmt.Fprintf(os.Stderr, "Kubeconfig written to %s\n", cfg.OutputPath)
	} else {
		fmt.Print(string(kubeconfigData))
	}

	if failed > 0 {
		return partialError(failed, len(kubeconfigs))
	}
	return nil
}

// applyGenerateFlags applies --prefix and --output on top of the environment.
// Flags().Changed is used so an explicit empty value (-o "" for stdout,
// -p "" for no prefix) overrides RANCHER_KUBECONFIG_OUTPUT and
// RANCHER_CLUSTER_PREFIX.
func applyGenerateFlags(cmd *cobra.Command, cfg *config.Config) {
	f := cmd.Flags()
	if f.Changed("prefix") {
		cfg.ClusterPrefix, _ = f.GetString("prefix")
	}
	if f.Changed("output") {
		cfg.OutputPath, _ = f.GetString("output")
	}
}
