package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kubeconfig-wrangler/pkg/config"
	"github.com/kubeconfig-wrangler/pkg/rancher"
)

// listCmd represents the list command
var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all clusters from Rancher",
	Long: `List all downstream Kubernetes clusters managed by the specified
Rancher instance, showing their name, ID, state, and provider.

Examples:
  # List all clusters using API token
  kubeconfig-wrangler list --url https://rancher.example.com --token token-xxxxx:yyyyyyy

  # List all clusters using username/password
  kubeconfig-wrangler list --url https://rancher.example.com --username admin --password mypassword

  # Using environment variables
  export RANCHER_URL=https://rancher.example.com
  export RANCHER_USERNAME=admin
  export RANCHER_PASSWORD=mypassword
  kubeconfig-wrangler list`,
	RunE: runList,
}

func init() {
	listCmd.Flags().StringVarP(&rancherURL, "url", "u", "", "Rancher server URL (env: RANCHER_URL)")
	listCmd.Flags().StringVarP(&accessKey, "access-key", "a", "", "Rancher API access key (env: RANCHER_ACCESS_KEY)")
	listCmd.Flags().StringVarP(&secretKey, "secret-key", "s", "", "Rancher API secret key (env: RANCHER_SECRET_KEY)")
	listCmd.Flags().StringVarP(&token, "token", "t", "", "Rancher API token (access_key:secret_key) (env: RANCHER_TOKEN)")
	listCmd.Flags().StringVar(&username, "username", "", "Rancher username for password auth (env: RANCHER_USERNAME)")
	listCmd.Flags().StringVar(&password, "password", "", "Rancher password for password auth (env: RANCHER_PASSWORD)")
	listCmd.Flags().BoolVarP(&insecureSkipTLS, "insecure-skip-tls-verify", "k", false, "Skip TLS certificate verification (env: RANCHER_INSECURE_SKIP_TLS_VERIFY)")
	listCmd.Flags().StringVar(&caCert, "ca-cert", "", "Path to CA certificate file (env: RANCHER_CA_CERT)")
}

func runList(cmd *cobra.Command, args []string) error {
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

	// List clusters
	clusters, err := client.ListClusters()
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}

	if len(clusters) == 0 {
		fmt.Println("No clusters found")
		return nil
	}

	// Print clusters in a table format
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tID\tSTATE\tPROVIDER")
	fmt.Fprintln(w, "----\t--\t-----\t--------")
	for _, cluster := range clusters {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", cluster.Name, cluster.ID, cluster.State, cluster.Provider)
	}
	w.Flush()

	return nil
}
