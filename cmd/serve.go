package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"

	"github.com/kubeconfig-wrangler/pkg/web"
)

var (
	serverAddr  string
	serverPort  int
	serverToken string
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
  kubeconfig-wrangler serve

  # Start the server on a custom port
  kubeconfig-wrangler serve --port 3000

The server prints a URL containing a random security token
("Open http://127.0.0.1:8080/?token=..."); open that URL to use the GUI.
Every request must carry the token.

WARNING: --addr defaults to 127.0.0.1. Binding to any other address (for
example 0.0.0.0) exposes the GUI, and through it all saved credentials, to
the network for anyone who obtains the token. Only do this on a trusted
network.`,
	RunE: runServe,
}

func init() {
	rootCmd.AddCommand(serveCmd)
	serveCmd.Flags().StringVar(&serverAddr, "addr", "127.0.0.1", "Address to bind the server to (non-loopback addresses expose the GUI to the network)")
	serveCmd.Flags().IntVar(&serverPort, "port", 8080, "Port to run the server on")
	serveCmd.Flags().StringVar(&serverToken, "token", "", "Security token for API authentication (env: "+serveTokenEnv+"; random if unset)")
}

// serveTokenEnv lets a parent process (the Electron shell) pass the token
// without exposing it in the process arguments.
const serveTokenEnv = "KUBECONFIG_WRANGLER_TOKEN"

// resolveServeToken returns the --token flag value, else the token from the
// environment, else a random 32-byte hex token. fromEnv reports whether the
// token came from the environment. The environment variable is always
// removed so child processes do not inherit it.
func resolveServeToken(flagToken string) (token string, fromEnv bool, err error) {
	envToken := os.Getenv(serveTokenEnv)
	_ = os.Unsetenv(serveTokenEnv)

	if flagToken != "" {
		return flagToken, false, nil
	}
	if envToken != "" {
		return envToken, true, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", false, fmt.Errorf("failed to generate security token: %w", err)
	}
	return hex.EncodeToString(b), false, nil
}

// serveOpenMessage is the startup line telling the user where to open the
// GUI. A token supplied privately by a parent process (Electron) is not
// printed, since the parent relays backend output to its own logs; a
// generated or --token token is printed because a human at a terminal needs
// the URL.
func serveOpenMessage(addr, token string, fromEnv bool) string {
	if fromEnv {
		return fmt.Sprintf("Open http://%s/ (token supplied by parent process)", addr)
	}
	return fmt.Sprintf("Open http://%s/?token=%s", addr, token)
}

func runServe(cmd *cobra.Command, args []string) error {
	token, fromEnv, err := resolveServeToken(serverToken)
	if err != nil {
		return err
	}
	addr := fmt.Sprintf("%s:%d", serverAddr, serverPort)
	log.Print(serveOpenMessage(addr, token, fromEnv))
	web.Version = Version
	server := web.NewServer(addr, token)
	return server.Start()
}
