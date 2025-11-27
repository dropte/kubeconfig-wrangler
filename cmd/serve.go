package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rancher-kubeconfig-proxy/pkg/web"
)

var (
	serverAddr string
	serverPort int
)

// serveCmd represents the serve command
var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the web GUI server",
	Long: `Start a local web server that provides a graphical user interface
for generating kubeconfig files from Rancher managed clusters.

The web interface allows you to:
  - Connect to a Rancher instance
  - View available clusters
  - Select which clusters to include
  - Configure cluster name prefix
  - Generate and download the kubeconfig

Examples:
  # Start the server on default port (8080)
  rancher-kubeconfig-proxy serve

  # Start the server on a custom port
  rancher-kubeconfig-proxy serve --port 3000

  # Start the server on a specific address
  rancher-kubeconfig-proxy serve --addr 0.0.0.0 --port 8080`,
	RunE: runServe,
}

func init() {
	rootCmd.AddCommand(serveCmd)
	serveCmd.Flags().StringVar(&serverAddr, "addr", "127.0.0.1", "Address to bind the server to")
	serveCmd.Flags().IntVar(&serverPort, "port", 8080, "Port to run the server on")
}

func runServe(cmd *cobra.Command, args []string) error {
	addr := fmt.Sprintf("%s:%d", serverAddr, serverPort)
	server := web.NewServer(addr)
	return server.Start()
}
