// Package cmd provides the CLI commands for rancher-kubeconfig-proxy
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
	Use:   "rancher-kubeconfig-proxy",
	Short: "Generate kubeconfig files from Rancher managed clusters",
	Long: `rancher-kubeconfig-proxy is a tool that connects to a Rancher instance
and generates a merged kubeconfig file containing all downstream Kubernetes
clusters managed by that Rancher instance.

The generated kubeconfig can be used by any standard Kubernetes tools like
kubectl, helm, k9s, and other applications that support kubeconfig files.

Cluster names can be prefixed with a configurable string to help identify
which Rancher instance they belong to.`,
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
	rootCmd.AddCommand(versionCmd)
}

// versionCmd prints the version
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("rancher-kubeconfig-proxy %s\n", Version)
	},
}
