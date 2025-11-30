package provider

import (
	"fmt"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// StaticConfig holds the configuration for a static kubeconfig provider
type StaticConfig struct {
	// ProfileID is the unique identifier for this profile
	ProfileID string

	// ProfileName is the display name for this profile
	ProfileName string

	// Kubeconfig is the raw kubeconfig content
	Kubeconfig string
}

// StaticProvider implements ClusterProvider for manually entered kubeconfigs
type StaticProvider struct {
	config   StaticConfig
	clusters []ClusterInfo
}

// NewStaticProvider creates a new static kubeconfig provider
func NewStaticProvider(config StaticConfig) (*StaticProvider, error) {
	if config.Kubeconfig == "" {
		return nil, fmt.Errorf("kubeconfig content is required")
	}

	provider := &StaticProvider{
		config: config,
	}

	// Parse the kubeconfig to extract cluster information
	if err := provider.parseKubeconfig(); err != nil {
		return nil, fmt.Errorf("failed to parse kubeconfig: %w", err)
	}

	return provider, nil
}

// parseKubeconfig parses the kubeconfig content and extracts cluster info
func (p *StaticProvider) parseKubeconfig() error {
	cfg, err := clientcmd.Load([]byte(p.config.Kubeconfig))
	if err != nil {
		return err
	}

	p.clusters = make([]ClusterInfo, 0, len(cfg.Contexts))

	for name, ctx := range cfg.Contexts {
		// Check if the cluster exists
		if _, exists := cfg.Clusters[ctx.Cluster]; !exists {
			continue
		}

		cluster := ClusterInfo{
			ID:          name, // Use context name as ID
			Name:        name,
			State:       "static",
			Provider:    "static",
			ProfileID:   p.config.ProfileID,
			ProfileName: p.config.ProfileName,
			Description: fmt.Sprintf("Cluster: %s, User: %s", ctx.Cluster, ctx.AuthInfo),
		}
		p.clusters = append(p.clusters, cluster)
	}

	return nil
}

// Name returns the display name of this provider instance
func (p *StaticProvider) Name() string {
	return p.config.ProfileName
}

// Type returns the provider type
func (p *StaticProvider) Type() string {
	return "static"
}

// ProfileID returns the profile ID
func (p *StaticProvider) ProfileID() string {
	return p.config.ProfileID
}

// ListClusters returns the clusters defined in the static kubeconfig
func (p *StaticProvider) ListClusters() ([]ClusterInfo, error) {
	return p.clusters, nil
}

// GetKubeconfig returns a kubeconfig for a specific context
func (p *StaticProvider) GetKubeconfig(clusterID string) (string, error) {
	cfg, err := clientcmd.Load([]byte(p.config.Kubeconfig))
	if err != nil {
		return "", err
	}

	// Check if the context exists
	ctx, exists := cfg.Contexts[clusterID]
	if !exists {
		return "", fmt.Errorf("context not found: %s", clusterID)
	}

	// Create a new kubeconfig with just this context
	newCfg := &clientcmdapi.Config{
		Clusters:       make(map[string]*clientcmdapi.Cluster),
		AuthInfos:      make(map[string]*clientcmdapi.AuthInfo),
		Contexts:       make(map[string]*clientcmdapi.Context),
		CurrentContext: clusterID,
	}
	newCfg.Contexts[clusterID] = ctx

	// Copy the referenced cluster
	if cluster, exists := cfg.Clusters[ctx.Cluster]; exists {
		newCfg.Clusters[ctx.Cluster] = cluster
	}

	// Copy the referenced auth info
	if authInfo, exists := cfg.AuthInfos[ctx.AuthInfo]; exists {
		newCfg.AuthInfos[ctx.AuthInfo] = authInfo
	}

	// Serialize the config
	content, err := clientcmd.Write(*newCfg)
	if err != nil {
		return "", fmt.Errorf("failed to serialize kubeconfig: %w", err)
	}

	return string(content), nil
}

// Close cleans up any resources held by the provider
func (p *StaticProvider) Close() error {
	return nil
}
