// Package cmd provides the CLI commands for kubeconfig-wrangler
package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	// Version is set during build
	Version = "dev"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "kubeconfig-wrangler",
	Short: "Generate kubeconfig files from multiple sources",
	Long: `kubeconfig-wrangler is a tool that connects to Rancher instances, AWS EKS,
and other sources to generate merged kubeconfig files containing all your
Kubernetes clusters.

The generated kubeconfig can be used by any standard Kubernetes tools like
kubectl, helm, k9s, and other applications that support kubeconfig files.

Cluster names can be prefixed with a configurable string to help identify
which source they belong to.`,
}

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(generateCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(eksCmd)
	rootCmd.AddCommand(versionCmd)
}

// versionCmd prints the version
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("kubeconfig-wrangler %s\n", Version)
	},
}
