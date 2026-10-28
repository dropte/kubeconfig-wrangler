package provider

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

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
		// A context is only usable when both its cluster and user exist.
		if contextProblem(cfg, ctx) != "" {
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
		if len(clusterRelativePaths(ctx.Cluster, cfg.Clusters[ctx.Cluster]))+
			len(authInfoRelativePaths(ctx.AuthInfo, cfg.AuthInfos[ctx.AuthInfo])) > 0 {
			cluster.Description += " (warning: references relative file paths and cannot be exported; use absolute paths or embedded data)"
		}
		p.clusters = append(p.clusters, cluster)
	}

	// Map iteration order is random; keep the list stable.
	sort.Slice(p.clusters, func(i, j int) bool { return p.clusters[i].Name < p.clusters[j].Name })

	return nil
}

// contextProblem returns a description of why ctx cannot be exported on its
// own, or "" when its cluster and user are both defined in cfg.
func contextProblem(cfg *clientcmdapi.Config, ctx *clientcmdapi.Context) string {
	if _, exists := cfg.Clusters[ctx.Cluster]; !exists {
		return fmt.Sprintf("cluster %q is not defined", ctx.Cluster)
	}
	if _, exists := cfg.AuthInfos[ctx.AuthInfo]; !exists {
		return fmt.Sprintf("user %q is not defined", ctx.AuthInfo)
	}
	return ""
}

// RelativePathProblems lists the file references in a kubeconfig that are
// relative paths: certificate-authority, client-certificate, client-key,
// tokenFile, and exec.command when it contains a path separator (a bare
// command name is resolved via PATH and is fine). A stored kubeconfig has no
// origin directory, so once merged into ~/.kube/config such paths resolve
// against ~/.kube and break. Callers saving a static profile can use this to
// reject or warn about the content.
func RelativePathProblems(kubeconfig string) ([]string, error) {
	cfg, err := clientcmd.Load([]byte(kubeconfig))
	if err != nil {
		return nil, err
	}
	var problems []string
	for _, name := range sortedKeys(cfg.Clusters) {
		problems = append(problems, clusterRelativePaths(name, cfg.Clusters[name])...)
	}
	for _, name := range sortedKeys(cfg.AuthInfos) {
		problems = append(problems, authInfoRelativePaths(name, cfg.AuthInfos[name])...)
	}
	return problems, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func isRelativePath(path string) bool {
	return path != "" && !filepath.IsAbs(path) && !strings.HasPrefix(path, "/")
}

func clusterRelativePaths(name string, c *clientcmdapi.Cluster) []string {
	if c != nil && isRelativePath(c.CertificateAuthority) {
		return []string{fmt.Sprintf("cluster %q certificate-authority %q", name, c.CertificateAuthority)}
	}
	return nil
}

func authInfoRelativePaths(name string, a *clientcmdapi.AuthInfo) []string {
	if a == nil {
		return nil
	}
	var problems []string
	for _, f := range []struct{ field, value string }{
		{"client-certificate", a.ClientCertificate},
		{"client-key", a.ClientKey},
		{"tokenFile", a.TokenFile},
	} {
		if isRelativePath(f.value) {
			problems = append(problems, fmt.Sprintf("user %q %s %q", name, f.field, f.value))
		}
	}
	if a.Exec != nil && strings.ContainsAny(a.Exec.Command, `/\`) && isRelativePath(a.Exec.Command) {
		problems = append(problems, fmt.Sprintf("user %q exec command %q", name, a.Exec.Command))
	}
	return problems
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
	if problem := contextProblem(cfg, ctx); problem != "" {
		return "", fmt.Errorf("context %s is incomplete: %s", clusterID, problem)
	}

	// Create a new kubeconfig with just this context
	newCfg := &clientcmdapi.Config{
		Clusters:       make(map[string]*clientcmdapi.Cluster),
		AuthInfos:      make(map[string]*clientcmdapi.AuthInfo),
		Contexts:       make(map[string]*clientcmdapi.Context),
		CurrentContext: clusterID,
	}
	newCfg.Contexts[clusterID] = ctx

	newCfg.Clusters[ctx.Cluster] = cfg.Clusters[ctx.Cluster]
	newCfg.AuthInfos[ctx.AuthInfo] = cfg.AuthInfos[ctx.AuthInfo]

	// Relative file references would resolve against the destination
	// directory (e.g. ~/.kube) after export and silently break.
	problems := append(clusterRelativePaths(ctx.Cluster, cfg.Clusters[ctx.Cluster]),
		authInfoRelativePaths(ctx.AuthInfo, cfg.AuthInfos[ctx.AuthInfo])...)
	if len(problems) > 0 {
		return "", fmt.Errorf("context %s references relative file paths (%s); "+
			"use absolute paths or embed the data (certificate-authority-data, client-certificate-data, client-key-data, token)",
			clusterID, strings.Join(problems, "; "))
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
