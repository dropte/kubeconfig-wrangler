package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

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
	registerRancherFlags(listCmd)
}

func runList(cmd *cobra.Command, args []string) error {
	// Build configuration from environment and flags
	cfg := rancherConfigFromFlags(cmd)

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
