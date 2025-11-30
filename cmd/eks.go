package cmd

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kubeconfig-wrangler/pkg/kubeconfig"
	"github.com/kubeconfig-wrangler/pkg/provider"
)

var (
	eksRegion       string
	eksProfile      string
	eksAccessKey    string
	eksSecretKey    string
	eksSessionToken string
	eksPrefix       string
	eksOutput       string
)

// eksCmd represents the eks command
var eksCmd = &cobra.Command{
	Use:   "eks",
	Short: "Manage AWS EKS clusters",
	Long: `Manage AWS EKS clusters - list clusters and generate kubeconfig files.

Authentication can be provided via:
  - AWS CLI profile (--profile or AWS_PROFILE)
  - Direct credentials (--access-key/--secret-key or AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY)
  - Default credentials (environment, instance profile, etc.)

Examples:
  # List EKS clusters in a specific region
  kubeconfig-wrangler eks list --region us-west-2

  # List EKS clusters using a specific AWS profile
  kubeconfig-wrangler eks list --region us-east-1 --profile my-profile

  # Generate kubeconfig for all EKS clusters in a region
  kubeconfig-wrangler eks generate --region us-west-2

  # Generate kubeconfig with cluster name prefix
  kubeconfig-wrangler eks generate --region us-west-2 --prefix "eks-"`,
}

// eksListCmd represents the eks list command
var eksListCmd = &cobra.Command{
	Use:   "list",
	Short: "List EKS clusters",
	Long: `List all EKS clusters in the specified AWS region.

Examples:
  # List clusters in us-west-2
  kubeconfig-wrangler eks list --region us-west-2

  # List clusters using a specific AWS profile
  kubeconfig-wrangler eks list --region us-east-1 --profile production

  # List clusters using direct credentials
  kubeconfig-wrangler eks list --region us-west-2 --access-key AKIA... --secret-key ...`,
	RunE: runEKSList,
}

// eksGenerateCmd represents the eks generate command
var eksGenerateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate kubeconfig from EKS clusters",
	Long: `Generate a merged kubeconfig file containing all EKS clusters
in the specified AWS region.

The kubeconfig uses the AWS CLI for authentication, so you need the
AWS CLI installed and configured to use the generated kubeconfig.

Examples:
  # Generate kubeconfig for all clusters in us-west-2
  kubeconfig-wrangler eks generate --region us-west-2

  # Generate kubeconfig with cluster name prefix
  kubeconfig-wrangler eks generate --region us-west-2 --prefix "prod-"

  # Generate kubeconfig to a specific file
  kubeconfig-wrangler eks generate --region us-west-2 --output ~/.kube/eks-config

  # Generate kubeconfig using a specific AWS profile
  kubeconfig-wrangler eks generate --region us-east-1 --profile my-profile`,
	RunE: runEKSGenerate,
}

// eksProfilesCmd lists available AWS profiles
var eksProfilesCmd = &cobra.Command{
	Use:   "profiles",
	Short: "List available AWS CLI profiles",
	Long:  `List all AWS CLI profiles configured in ~/.aws/credentials and ~/.aws/config.`,
	RunE:  runEKSProfiles,
}

// eksRegionsCmd lists common AWS regions
var eksRegionsCmd = &cobra.Command{
	Use:   "regions",
	Short: "List common AWS regions",
	Long:  `List common AWS regions where EKS clusters can be created.`,
	Run:   runEKSRegions,
}

func init() {
	// Common flags for all EKS commands
	eksCmd.PersistentFlags().StringVarP(&eksRegion, "region", "r", "", "AWS region (env: AWS_REGION or AWS_DEFAULT_REGION)")
	eksCmd.PersistentFlags().StringVar(&eksProfile, "profile", "", "AWS CLI profile name (env: AWS_PROFILE)")
	eksCmd.PersistentFlags().StringVar(&eksAccessKey, "access-key", "", "AWS access key ID (env: AWS_ACCESS_KEY_ID)")
	eksCmd.PersistentFlags().StringVar(&eksSecretKey, "secret-key", "", "AWS secret access key (env: AWS_SECRET_ACCESS_KEY)")
	eksCmd.PersistentFlags().StringVar(&eksSessionToken, "session-token", "", "AWS session token (env: AWS_SESSION_TOKEN)")

	// Generate-specific flags
	eksGenerateCmd.Flags().StringVarP(&eksPrefix, "prefix", "p", "", "Prefix to add to cluster names")
	eksGenerateCmd.Flags().StringVarP(&eksOutput, "output", "o", "", "Output file path (default: stdout)")

	// Add subcommands
	eksCmd.AddCommand(eksListCmd)
	eksCmd.AddCommand(eksGenerateCmd)
	eksCmd.AddCommand(eksProfilesCmd)
	eksCmd.AddCommand(eksRegionsCmd)
}

func getEKSConfigFromFlags() provider.EKSConfig {
	cfg := provider.EKSConfig{
		ProfileID:   "cli",
		ProfileName: "CLI",
		Region:      eksRegion,
		AWSProfile:  eksProfile,
		AccessKey:   eksAccessKey,
		SecretKey:   eksSecretKey,
	}

	// Load from environment if not specified via flags
	if cfg.Region == "" {
		cfg.Region = os.Getenv("AWS_REGION")
		if cfg.Region == "" {
			cfg.Region = os.Getenv("AWS_DEFAULT_REGION")
		}
	}
	if cfg.AWSProfile == "" {
		cfg.AWSProfile = os.Getenv("AWS_PROFILE")
	}
	if cfg.AccessKey == "" {
		cfg.AccessKey = os.Getenv("AWS_ACCESS_KEY_ID")
	}
	if cfg.SecretKey == "" {
		cfg.SecretKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
	if eksSessionToken != "" {
		cfg.SessionToken = eksSessionToken
	} else {
		cfg.SessionToken = os.Getenv("AWS_SESSION_TOKEN")
	}

	return cfg
}

func runEKSList(cmd *cobra.Command, args []string) error {
	cfg := getEKSConfigFromFlags()

	if cfg.Region == "" {
		return fmt.Errorf("region is required (use --region or set AWS_REGION)")
	}

	// Create EKS provider
	eksProvider, err := provider.NewEKSProvider(cfg)
	if err != nil {
		return fmt.Errorf("failed to create EKS provider: %w", err)
	}
	defer eksProvider.Close()

	// List clusters
	fmt.Fprintf(os.Stderr, "Fetching EKS clusters from %s...\n", cfg.Region)
	clusters, err := eksProvider.ListClusters()
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}

	if len(clusters) == 0 {
		fmt.Printf("No EKS clusters found in %s\n", cfg.Region)
		return nil
	}

	// Print clusters in a table format
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tREGION\tARN")
	fmt.Fprintln(w, "----\t------\t------\t---")
	for _, cluster := range clusters {
		arn := cluster.Description
		if len(arn) > 60 {
			arn = arn[:57] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", cluster.Name, cluster.State, cluster.Region, arn)
	}
	w.Flush()

	return nil
}

func runEKSGenerate(cmd *cobra.Command, args []string) error {
	cfg := getEKSConfigFromFlags()

	if cfg.Region == "" {
		return fmt.Errorf("region is required (use --region or set AWS_REGION)")
	}

	// Create EKS provider
	eksProvider, err := provider.NewEKSProvider(cfg)
	if err != nil {
		return fmt.Errorf("failed to create EKS provider: %w", err)
	}
	defer eksProvider.Close()

	// List clusters
	fmt.Fprintf(os.Stderr, "Fetching EKS clusters from %s...\n", cfg.Region)
	clusters, err := eksProvider.ListClusters()
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}

	if len(clusters) == 0 {
		return fmt.Errorf("no EKS clusters found in %s", cfg.Region)
	}

	fmt.Fprintf(os.Stderr, "Found %d cluster(s)\n", len(clusters))

	// Get kubeconfigs for each cluster
	kubeconfigs := make(map[string]string)
	for _, cluster := range clusters {
		if !strings.EqualFold(cluster.State, "ACTIVE") {
			fmt.Fprintf(os.Stderr, "Skipping cluster %s (status: %s)\n", cluster.Name, cluster.State)
			continue
		}

		kubeconfigYAML, err := eksProvider.GetKubeconfig(cluster.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to get kubeconfig for %s: %v\n", cluster.Name, err)
			continue
		}

		kubeconfigs[cluster.Name] = kubeconfigYAML
	}

	if len(kubeconfigs) == 0 {
		return fmt.Errorf("no active clusters found")
	}

	// Generate merged kubeconfig
	generator := kubeconfig.NewGenerator(eksPrefix)
	kubeconfigData, err := generator.Generate(kubeconfigs)
	if err != nil {
		return fmt.Errorf("failed to generate kubeconfig: %w", err)
	}

	// Output the kubeconfig
	if eksOutput != "" {
		if err := os.WriteFile(eksOutput, kubeconfigData, 0600); err != nil {
			return fmt.Errorf("failed to write kubeconfig to %s: %w", eksOutput, err)
		}
		fmt.Fprintf(os.Stderr, "Kubeconfig written to %s\n", eksOutput)
	} else {
		fmt.Print(string(kubeconfigData))
	}

	return nil
}

func runEKSProfiles(cmd *cobra.Command, args []string) error {
	profiles, err := provider.ListAWSProfiles()
	if err != nil {
		return fmt.Errorf("failed to list AWS profiles: %w", err)
	}

	if len(profiles) == 0 {
		fmt.Println("No AWS profiles found in ~/.aws/credentials or ~/.aws/config")
		return nil
	}

	fmt.Println("Available AWS profiles:")
	for _, profile := range profiles {
		fmt.Printf("  %s\n", profile)
	}

	return nil
}

func runEKSRegions(cmd *cobra.Command, args []string) {
	regions := provider.ListAWSRegions()

	fmt.Println("Common AWS regions:")
	for _, region := range regions {
		fmt.Printf("  %s\n", region)
	}
}
