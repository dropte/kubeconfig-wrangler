package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"gopkg.in/ini.v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// EKSConfig holds the configuration for an EKS provider
type EKSConfig struct {
	// ProfileID is the unique identifier for this profile
	ProfileID string

	// ProfileName is the display name for this profile
	ProfileName string

	// Region is the AWS region
	Region string

	// AWSProfile is the AWS CLI profile name (from ~/.aws/credentials)
	AWSProfile string

	// AccessKey is the AWS access key ID (for direct credentials)
	AccessKey string

	// SecretKey is the AWS secret access key (for direct credentials)
	SecretKey string

	// SessionToken is an optional AWS session token
	SessionToken string
}

// EKSProvider implements ClusterProvider for AWS EKS
type EKSProvider struct {
	config    EKSConfig
	eksClient *eks.Client
}

// NewEKSProvider creates a new EKS provider instance
func NewEKSProvider(eksConfig EKSConfig) (*EKSProvider, error) {
	ctx := context.Background()

	var cfg aws.Config
	var err error

	if eksConfig.AccessKey != "" && eksConfig.SecretKey != "" {
		// Use direct credentials
		creds := credentials.NewStaticCredentialsProvider(
			eksConfig.AccessKey,
			eksConfig.SecretKey,
			eksConfig.SessionToken,
		)
		cfg, err = config.LoadDefaultConfig(ctx,
			config.WithRegion(eksConfig.Region),
			config.WithCredentialsProvider(creds),
		)
	} else if eksConfig.AWSProfile != "" {
		// Use AWS CLI profile
		cfg, err = config.LoadDefaultConfig(ctx,
			config.WithRegion(eksConfig.Region),
			config.WithSharedConfigProfile(eksConfig.AWSProfile),
		)
	} else {
		// Use default credentials (environment, instance profile, etc.)
		cfg, err = config.LoadDefaultConfig(ctx,
			config.WithRegion(eksConfig.Region),
		)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &EKSProvider{
		config:    eksConfig,
		eksClient: eks.NewFromConfig(cfg),
	}, nil
}

// Name returns the display name of this provider instance
func (p *EKSProvider) Name() string {
	return p.config.ProfileName
}

// Type returns the provider type
func (p *EKSProvider) Type() string {
	return "eks"
}

// ProfileID returns the profile ID
func (p *EKSProvider) ProfileID() string {
	return p.config.ProfileID
}

// EKSDescribeFailedPrefix starts the Description of a cluster that
// ListClusters kept although its DescribeCluster call failed.
const EKSDescribeFailedPrefix = "describe failed: "

// eksCallTimeout bounds each individual EKS API call.
const eksCallTimeout = 30 * time.Second

// ListClusters retrieves all EKS clusters in the configured region, sorted by
// name. A cluster whose DescribeCluster call fails (throttling, AccessDenied,
// 5xx) is still listed, with State "unknown" and the error in Description, so
// it stays visible and selectable. An error is returned only when every
// DescribeCluster call failed.
func (p *EKSProvider) ListClusters() ([]ClusterInfo, error) {
	names, err := p.listClusterNames()
	if err != nil {
		return nil, err
	}

	var clusters []ClusterInfo
	var errs []error
	for _, name := range names {
		cluster, err := p.describeCluster(name)
		if err != nil {
			log.Printf("EKS: failed to describe cluster %s: %v", name, err)
			errs = append(errs, fmt.Errorf("cluster %s: %w", name, err))
			cluster = ClusterInfo{
				ID:          name,
				Name:        name,
				State:       "unknown",
				Provider:    "eks",
				ProfileID:   p.config.ProfileID,
				ProfileName: p.config.ProfileName,
				Region:      p.config.Region,
				Description: EKSDescribeFailedPrefix + err.Error(),
			}
		}
		clusters = append(clusters, cluster)
	}

	if len(names) > 0 && len(errs) == len(names) {
		return nil, fmt.Errorf("failed to describe any EKS cluster: %w", errors.Join(errs...))
	}

	sort.Slice(clusters, func(i, j int) bool { return clusters[i].Name < clusters[j].Name })
	return clusters, nil
}

// listClusterNames pages through ListClusters, giving each page its own timeout.
func (p *EKSProvider) listClusterNames() ([]string, error) {
	var names []string
	var nextToken *string
	for {
		ctx, cancel := context.WithTimeout(context.Background(), eksCallTimeout)
		output, err := p.eksClient.ListClusters(ctx, &eks.ListClustersInput{
			NextToken: nextToken,
		})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("failed to list EKS clusters: %w", err)
		}
		names = append(names, output.Clusters...)

		if output.NextToken == nil {
			return names, nil
		}
		nextToken = output.NextToken
	}
}

// describeCluster fetches one cluster's details with its own timeout.
func (p *EKSProvider) describeCluster(name string) (ClusterInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), eksCallTimeout)
	defer cancel()

	describe, err := p.eksClient.DescribeCluster(ctx, &eks.DescribeClusterInput{
		Name: aws.String(name),
	})
	if err != nil {
		return ClusterInfo{}, err
	}

	cluster := ClusterInfo{
		ID:          name,
		Name:        name,
		State:       "unknown",
		Provider:    "eks",
		ProfileID:   p.config.ProfileID,
		ProfileName: p.config.ProfileName,
		Region:      p.config.Region,
	}
	if describe.Cluster != nil {
		if describe.Cluster.Status != "" {
			cluster.State = string(describe.Cluster.Status)
		}
		if describe.Cluster.Arn != nil {
			cluster.Description = *describe.Cluster.Arn
		}
	}
	return cluster, nil
}

// GetKubeconfig generates a kubeconfig for a specific EKS cluster
func (p *EKSProvider) GetKubeconfig(clusterID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), eksCallTimeout)
	defer cancel()

	describe, err := p.eksClient.DescribeCluster(ctx, &eks.DescribeClusterInput{
		Name: aws.String(clusterID),
	})
	if err != nil {
		return "", fmt.Errorf("failed to describe cluster: %w", err)
	}

	cluster := describe.Cluster
	if cluster == nil || cluster.Endpoint == nil || cluster.CertificateAuthority == nil || cluster.CertificateAuthority.Data == nil {
		return "", fmt.Errorf("cluster %s is missing required fields", clusterID)
	}

	// Generate kubeconfig that uses aws eks get-token for authentication
	return p.generateKubeconfig(
		clusterID,
		*cluster.Endpoint,
		*cluster.CertificateAuthority.Data,
	)
}

// EmbedsCredentials reports whether generated kubeconfigs contain the static
// AWS access key, secret key and (if set) session token in plain text.
func (p *EKSProvider) EmbedsCredentials() bool {
	return p.config.AccessKey != "" && p.config.SecretKey != ""
}

// generateKubeconfig creates a kubeconfig YAML for an EKS cluster.
// caData is the base64-encoded CA bundle as returned by DescribeCluster.
func (p *EKSProvider) generateKubeconfig(clusterName, endpoint, caData string) (string, error) {
	ca, err := base64.StdEncoding.DecodeString(caData)
	if err != nil {
		return "", fmt.Errorf("cluster %s has invalid certificate authority data: %w", clusterName, err)
	}

	// Build the aws CLI command for authentication
	// Follow NewEKSProvider's precedence: static keys win over the profile.
	// An explicit --profile would disable the aws CLI's environment
	// credential provider, so it is only passed when no keys are embedded.
	var args []string
	if !p.EmbedsCredentials() && p.config.AWSProfile != "" {
		args = append(args, "--profile", p.config.AWSProfile)
	}
	args = append(args, "eks", "get-token", "--cluster-name", clusterName, "--region", p.config.Region)

	exec := &clientcmdapi.ExecConfig{
		APIVersion:      "client.authentication.k8s.io/v1beta1",
		Command:         "aws",
		Args:            args,
		InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
	}

	// Direct credentials are passed to the aws CLI through environment variables.
	if p.EmbedsCredentials() {
		exec.Env = []clientcmdapi.ExecEnvVar{
			{Name: "AWS_ACCESS_KEY_ID", Value: p.config.AccessKey},
			{Name: "AWS_SECRET_ACCESS_KEY", Value: p.config.SecretKey},
		}
		if p.config.SessionToken != "" {
			exec.Env = append(exec.Env, clientcmdapi.ExecEnvVar{Name: "AWS_SESSION_TOKEN", Value: p.config.SessionToken})
		}
	}

	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[clusterName] = &clientcmdapi.Cluster{
		Server:                   endpoint,
		CertificateAuthorityData: ca,
	}
	cfg.AuthInfos[clusterName] = &clientcmdapi.AuthInfo{Exec: exec}
	cfg.Contexts[clusterName] = &clientcmdapi.Context{
		Cluster:  clusterName,
		AuthInfo: clusterName,
	}
	cfg.CurrentContext = clusterName

	data, err := clientcmd.Write(*cfg)
	if err != nil {
		return "", fmt.Errorf("failed to serialize kubeconfig for cluster %s: %w", clusterName, err)
	}
	return string(data), nil
}

// Close cleans up any resources held by the provider
func (p *EKSProvider) Close() error {
	return nil
}

// ListAWSProfiles returns the sorted list of AWS CLI profiles defined in the
// shared credentials and config files. AWS_SHARED_CREDENTIALS_FILE and
// AWS_CONFIG_FILE override the default ~/.aws locations.
func ListAWSProfiles() ([]string, error) {
	credentialsPath := os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if credentialsPath == "" {
		credentialsPath = config.DefaultSharedCredentialsFilename()
	}
	configPath := os.Getenv("AWS_CONFIG_FILE")
	if configPath == "" {
		configPath = config.DefaultSharedConfigFilename()
	}

	seen := make(map[string]bool)

	// Credentials file sections are bare profile names.
	for _, section := range loadAWSIniSections(credentialsPath) {
		if name := strings.TrimSpace(section); name != "" {
			seen[name] = true
		}
	}

	// Config file sections are "[default]" or "[profile <name>]"; other
	// section kinds (sso-session, services, ...) are not profiles.
	for _, section := range loadAWSIniSections(configPath) {
		name := strings.TrimSpace(section)
		switch {
		case name == "default":
			seen["default"] = true
		case strings.HasPrefix(name, "profile ") || strings.HasPrefix(name, "profile\t"):
			if profile := strings.TrimSpace(name[len("profile"):]); profile != "" {
				seen[profile] = true
			}
		}
	}

	profiles := make([]string, 0, len(seen))
	for name := range seen {
		profiles = append(profiles, name)
	}
	sort.Strings(profiles)
	return profiles, nil
}

// loadAWSIniSections returns the section names of an AWS ini file. A missing
// file yields nothing; a parse error is logged and yields nothing.
func loadAWSIniSections(path string) []string {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	cfg, err := ini.LoadSources(ini.LoadOptions{AllowNestedValues: true}, path)
	if err != nil {
		log.Printf("Warning: failed to parse AWS file %s: %v", path, err)
		return nil
	}
	var names []string
	for _, section := range cfg.Sections() {
		if name := section.Name(); name != ini.DefaultSection {
			names = append(names, name)
		}
	}
	return names
}

// ListAWSRegions returns a list of common AWS regions
func ListAWSRegions() []string {
	return []string{
		"us-east-1",
		"us-east-2",
		"us-west-1",
		"us-west-2",
		"af-south-1",
		"ap-east-1",
		"ap-south-1",
		"ap-south-2",
		"ap-northeast-1",
		"ap-northeast-2",
		"ap-northeast-3",
		"ap-southeast-1",
		"ap-southeast-2",
		"ap-southeast-3",
		"ap-southeast-4",
		"ca-central-1",
		"ca-west-1",
		"eu-central-1",
		"eu-central-2",
		"eu-west-1",
		"eu-west-2",
		"eu-west-3",
		"eu-south-1",
		"eu-south-2",
		"eu-north-1",
		"il-central-1",
		"me-south-1",
		"me-central-1",
		"sa-east-1",
	}
}
