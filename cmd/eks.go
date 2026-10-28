package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
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

Credentials come from exactly one source, in this order: --access-key/--secret-key
(with --session-token), then --profile or AWS_PROFILE, then the AWS_* key
environment variables. Direct credentials are embedded in plain text in the
generated kubeconfig; prefer a profile where possible.

The region comes from --region, AWS_REGION/AWS_DEFAULT_REGION, or the region
configured for the AWS profile.

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
	Long: `List all AWS CLI profiles configured in ~/.aws/credentials and ~/.aws/config
(or the files named by AWS_SHARED_CREDENTIALS_FILE and AWS_CONFIG_FILE).`,
	RunE: runEKSProfiles,
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

// eksFlagValues holds the raw EKS credential and region flags.
type eksFlagValues struct {
	region, profile, accessKey, secretKey, sessionToken string
}

// resolveEKSCredentials builds the EKS configuration, taking credentials from
// exactly one source: key flags (session token only from --session-token),
// else a profile (--profile or AWS_PROFILE) with no static keys, else the
// AWS_* key environment variables together with AWS_SESSION_TOKEN.
// The region is taken from the flag or environment; it may still be empty.
func resolveEKSCredentials(f eksFlagValues, getenv func(string) string) (provider.EKSConfig, error) {
	cfg := provider.EKSConfig{
		ProfileID:   "cli",
		ProfileName: "CLI",
		Region:      f.region,
	}
	if cfg.Region == "" {
		cfg.Region = getenv("AWS_REGION")
	}
	if cfg.Region == "" {
		cfg.Region = getenv("AWS_DEFAULT_REGION")
	}

	keyFlags := f.accessKey != "" || f.secretKey != ""
	switch {
	case f.profile != "" && keyFlags:
		return cfg, errors.New("--profile cannot be combined with --access-key/--secret-key; use one credential source")
	case keyFlags:
		if f.accessKey == "" || f.secretKey == "" {
			return cfg, errors.New("--access-key and --secret-key must be given together")
		}
		cfg.AccessKey, cfg.SecretKey, cfg.SessionToken = f.accessKey, f.secretKey, f.sessionToken
	case f.sessionToken != "":
		return cfg, errors.New("--session-token requires --access-key and --secret-key")
	case f.profile != "" || getenv("AWS_PROFILE") != "":
		cfg.AWSProfile = f.profile
		if cfg.AWSProfile == "" {
			cfg.AWSProfile = getenv("AWS_PROFILE")
		}
	default:
		if ak, sk := getenv("AWS_ACCESS_KEY_ID"), getenv("AWS_SECRET_ACCESS_KEY"); ak != "" && sk != "" {
			cfg.AccessKey, cfg.SecretKey, cfg.SessionToken = ak, sk, getenv("AWS_SESSION_TOKEN")
		}
	}
	return cfg, nil
}

// resolveEKSRegion fills in cfg.Region from the AWS shared config (honouring
// the selected profile) when neither the flag nor the environment set it.
func resolveEKSRegion(cfg *provider.EKSConfig) error {
	if cfg.Region == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var opts []func(*awsconfig.LoadOptions) error
		if cfg.AWSProfile != "" {
			opts = append(opts, awsconfig.WithSharedConfigProfile(cfg.AWSProfile))
		}
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			return fmt.Errorf("failed to load AWS config: %w", err)
		}
		cfg.Region = awsCfg.Region
	}
	if cfg.Region == "" {
		return errors.New("region is required (use --region, set AWS_REGION, or configure a region for the AWS profile)")
	}
	return nil
}

// getEKSConfigFromFlags resolves the EKS configuration from flags, the
// environment and the AWS shared config.
func getEKSConfigFromFlags() (provider.EKSConfig, error) {
	cfg, err := resolveEKSCredentials(eksFlagValues{
		region:       eksRegion,
		profile:      eksProfile,
		accessKey:    eksAccessKey,
		secretKey:    eksSecretKey,
		sessionToken: eksSessionToken,
	}, os.Getenv)
	if err != nil {
		return cfg, err
	}
	if err := resolveEKSRegion(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func runEKSList(cmd *cobra.Command, args []string) error {
	cfg, err := getEKSConfigFromFlags()
	if err != nil {
		return err
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
	var describeErrs []error
	for _, cluster := range clusters {
		arn := cluster.Description
		if strings.HasPrefix(arn, provider.EKSDescribeFailedPrefix) {
			describeErrs = append(describeErrs, fmt.Errorf("cluster %s: %s", cluster.Name, arn))
			arn = "(describe failed, see warning)"
		}
		if len(arn) > 60 {
			arn = arn[:57] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", cluster.Name, cluster.State, cluster.Region, arn)
	}
	w.Flush()

	if failed := reportClusterFailures(errors.Join(describeErrs...), len(clusters)-len(describeErrs)); failed > 0 {
		return fmt.Errorf("%d of %d clusters could not be described", failed, len(clusters))
	}
	return nil
}

func runEKSGenerate(cmd *cobra.Command, args []string) error {
	cfg, err := getEKSConfigFromFlags()
	if err != nil {
		return err
	}

	// Create EKS provider
	eksProvider, err := provider.NewEKSProvider(cfg)
	if err != nil {
		return fmt.Errorf("failed to create EKS provider: %w", err)
	}
	defer eksProvider.Close()

	if eksProvider.EmbedsCredentials() {
		fmt.Fprintln(os.Stderr, "Warning: the generated kubeconfig contains your AWS secret access key (and session token, if any) in plain text; use --profile to avoid this")
	}

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
	var fetchErrs []error
	for _, cluster := range clusters {
		if strings.HasPrefix(cluster.Description, provider.EKSDescribeFailedPrefix) {
			fetchErrs = append(fetchErrs, fmt.Errorf("cluster %s: %s", cluster.Name, cluster.Description))
			continue
		}
		if !strings.EqualFold(cluster.State, "ACTIVE") {
			fmt.Fprintf(os.Stderr, "Skipping cluster %s (status: %s)\n", cluster.Name, cluster.State)
			continue
		}

		kubeconfigYAML, err := eksProvider.GetKubeconfig(cluster.ID)
		if err != nil {
			fetchErrs = append(fetchErrs, fmt.Errorf("cluster %s: %w", cluster.Name, err))
			continue
		}

		kubeconfigs[cluster.Name] = kubeconfigYAML
	}

	failed := reportClusterFailures(errors.Join(fetchErrs...), len(kubeconfigs))
	if len(kubeconfigs) == 0 {
		if failed > 0 {
			return fmt.Errorf("failed to fetch a kubeconfig for any of the %d active cluster(s)", failed)
		}
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
		if err := writeOutputFile(eksOutput, kubeconfigData); err != nil {
			return fmt.Errorf("failed to write kubeconfig to %s: %w", eksOutput, err)
		}
		fmt.Fprintf(os.Stderr, "Kubeconfig written to %s\n", eksOutput)
	} else {
		fmt.Print(string(kubeconfigData))
	}

	if failed > 0 {
		return partialError(failed, len(kubeconfigs))
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
