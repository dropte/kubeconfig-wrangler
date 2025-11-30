package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"gopkg.in/ini.v1"
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

// ListClusters retrieves all EKS clusters in the configured region
func (p *EKSProvider) ListClusters() ([]ClusterInfo, error) {
	// Add timeout to prevent hanging
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var clusters []ClusterInfo
	var nextToken *string

	for {
		output, err := p.eksClient.ListClusters(ctx, &eks.ListClustersInput{
			NextToken: nextToken,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list EKS clusters: %w", err)
		}

		for _, name := range output.Clusters {
			// Get cluster details
			describe, err := p.eksClient.DescribeCluster(ctx, &eks.DescribeClusterInput{
				Name: aws.String(name),
			})
			if err != nil {
				// Log warning but continue
				continue
			}

			state := "unknown"
			if describe.Cluster.Status != "" {
				state = string(describe.Cluster.Status)
			}

			cluster := ClusterInfo{
				ID:          name,
				Name:        name,
				State:       state,
				Provider:    "eks",
				ProfileID:   p.config.ProfileID,
				ProfileName: p.config.ProfileName,
				Region:      p.config.Region,
			}

			if describe.Cluster.Arn != nil {
				cluster.Description = *describe.Cluster.Arn
			}

			clusters = append(clusters, cluster)
		}

		if output.NextToken == nil {
			break
		}
		nextToken = output.NextToken
	}

	return clusters, nil
}

// GetKubeconfig generates a kubeconfig for a specific EKS cluster
func (p *EKSProvider) GetKubeconfig(clusterID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	describe, err := p.eksClient.DescribeCluster(ctx, &eks.DescribeClusterInput{
		Name: aws.String(clusterID),
	})
	if err != nil {
		return "", fmt.Errorf("failed to describe cluster: %w", err)
	}

	cluster := describe.Cluster
	if cluster.Endpoint == nil || cluster.CertificateAuthority == nil || cluster.CertificateAuthority.Data == nil {
		return "", fmt.Errorf("cluster %s is missing required fields", clusterID)
	}

	// Generate kubeconfig that uses aws eks get-token for authentication
	kubeconfig := p.generateKubeconfig(
		clusterID,
		*cluster.Endpoint,
		*cluster.CertificateAuthority.Data,
	)

	return kubeconfig, nil
}

// generateKubeconfig creates a kubeconfig YAML for an EKS cluster
func (p *EKSProvider) generateKubeconfig(clusterName, endpoint, caData string) string {
	// Build the aws CLI command for authentication
	var awsArgs []string

	if p.config.AWSProfile != "" {
		awsArgs = append(awsArgs, "--profile", p.config.AWSProfile)
	}

	awsArgs = append(awsArgs, "eks", "get-token", "--cluster-name", clusterName, "--region", p.config.Region)

	// Build the exec command
	execCommand := "aws"
	execArgs := strings.Join(awsArgs, "\n            - ")

	// If using direct credentials, we need to set environment variables
	var execEnv string
	if p.config.AccessKey != "" && p.config.SecretKey != "" {
		execEnv = fmt.Sprintf(`
          env:
            - name: AWS_ACCESS_KEY_ID
              value: %s
            - name: AWS_SECRET_ACCESS_KEY
              value: %s`, p.config.AccessKey, p.config.SecretKey)
		if p.config.SessionToken != "" {
			execEnv += fmt.Sprintf(`
            - name: AWS_SESSION_TOKEN
              value: %s`, p.config.SessionToken)
		}
	}

	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
  - name: %s
    cluster:
      server: %s
      certificate-authority-data: %s
contexts:
  - name: %s
    context:
      cluster: %s
      user: %s
current-context: %s
users:
  - name: %s
    user:
      exec:
        apiVersion: client.authentication.k8s.io/v1beta1
        command: %s
        args:
            - %s%s
        interactiveMode: Never
`,
		clusterName,
		endpoint,
		caData,
		clusterName,
		clusterName,
		clusterName,
		clusterName,
		clusterName,
		execCommand,
		execArgs,
		execEnv,
	)

	return kubeconfig
}

// Close cleans up any resources held by the provider
func (p *EKSProvider) Close() error {
	return nil
}

// ListAWSProfiles returns the list of AWS CLI profiles configured on the system
func ListAWSProfiles() ([]string, error) {
	var profiles []string

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get home directory: %w", err)
	}

	// Check credentials file
	credentialsPath := filepath.Join(homeDir, ".aws", "credentials")
	if _, err := os.Stat(credentialsPath); err == nil {
		cfg, err := ini.Load(credentialsPath)
		if err == nil {
			for _, section := range cfg.Sections() {
				name := section.Name()
				if name != "DEFAULT" && name != "" {
					profiles = append(profiles, name)
				}
			}
		}
	}

	// Check config file for additional profiles
	configPath := filepath.Join(homeDir, ".aws", "config")
	if _, err := os.Stat(configPath); err == nil {
		cfg, err := ini.Load(configPath)
		if err == nil {
			for _, section := range cfg.Sections() {
				name := section.Name()
				// Config file uses "profile <name>" format
				if strings.HasPrefix(name, "profile ") {
					profileName := strings.TrimPrefix(name, "profile ")
					// Avoid duplicates
					found := false
					for _, p := range profiles {
						if p == profileName {
							found = true
							break
						}
					}
					if !found {
						profiles = append(profiles, profileName)
					}
				}
			}
		}
	}

	// Always include "default" if credentials exist
	if len(profiles) > 0 {
		hasDefault := false
		for _, p := range profiles {
			if p == "default" {
				hasDefault = true
				break
			}
		}
		if !hasDefault {
			profiles = append([]string{"default"}, profiles...)
		}
	}

	return profiles, nil
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

