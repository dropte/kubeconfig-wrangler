// Package kubeconfig provides functionality for generating and merging kubeconfig files
package kubeconfig

import (
	"fmt"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// Generator handles kubeconfig generation and merging
type Generator struct {
	prefix string
}

// NewGenerator creates a new kubeconfig generator with the specified cluster name prefix
func NewGenerator(prefix string) *Generator {
	return &Generator{
		prefix: prefix,
	}
}

// ParseKubeconfig parses a kubeconfig string into a client-go Config object
func (g *Generator) ParseKubeconfig(data string) (*api.Config, error) {
	config, err := clientcmd.Load([]byte(data))
	if err != nil {
		return nil, fmt.Errorf("failed to parse kubeconfig: %w", err)
	}
	return config, nil
}

// ApplyPrefix applies the configured prefix to all cluster, context, and user names in the config
func (g *Generator) ApplyPrefix(config *api.Config, clusterName string) *api.Config {
	if g.prefix == "" {
		return config
	}

	prefixedName := fmt.Sprintf("%s%s", g.prefix, clusterName)

	// Create new maps with prefixed names
	newClusters := make(map[string]*api.Cluster)
	newContexts := make(map[string]*api.Context)
	newAuthInfos := make(map[string]*api.AuthInfo)

	// Rename clusters
	for name, cluster := range config.Clusters {
		newName := prefixedName
		if name != clusterName {
			// If the original name doesn't match the cluster name, preserve some uniqueness
			newName = fmt.Sprintf("%s%s", g.prefix, name)
		}
		newClusters[newName] = cluster
	}

	// Rename auth infos (users)
	for name, authInfo := range config.AuthInfos {
		newName := fmt.Sprintf("%s%s", g.prefix, name)
		newAuthInfos[newName] = authInfo
	}

	// Rename and update contexts
	for name, context := range config.Contexts {
		newContextName := prefixedName
		if name != clusterName {
			newContextName = fmt.Sprintf("%s%s", g.prefix, name)
		}

		// Create a copy of the context with updated references
		newContext := context.DeepCopy()

		// Update cluster reference
		if _, exists := config.Clusters[context.Cluster]; exists {
			if context.Cluster == clusterName {
				newContext.Cluster = prefixedName
			} else {
				newContext.Cluster = fmt.Sprintf("%s%s", g.prefix, context.Cluster)
			}
		}

		// Update auth info reference
		if context.AuthInfo != "" {
			newContext.AuthInfo = fmt.Sprintf("%s%s", g.prefix, context.AuthInfo)
		}

		newContexts[newContextName] = newContext
	}

	// Update current context
	newCurrentContext := config.CurrentContext
	if config.CurrentContext != "" {
		if config.CurrentContext == clusterName {
			newCurrentContext = prefixedName
		} else {
			newCurrentContext = fmt.Sprintf("%s%s", g.prefix, config.CurrentContext)
		}
	}

	return &api.Config{
		Kind:           config.Kind,
		APIVersion:     config.APIVersion,
		Clusters:       newClusters,
		Contexts:       newContexts,
		AuthInfos:      newAuthInfos,
		CurrentContext: newCurrentContext,
		Preferences:    config.Preferences,
		Extensions:     config.Extensions,
	}
}

// MergeConfigs merges multiple kubeconfig strings into a single config
// The clusterKubeconfigs map has cluster names as keys and kubeconfig YAML strings as values
func (g *Generator) MergeConfigs(clusterKubeconfigs map[string]string) (*api.Config, error) {
	mergedConfig := api.NewConfig()

	for clusterName, kubeconfigData := range clusterKubeconfigs {
		config, err := g.ParseKubeconfig(kubeconfigData)
		if err != nil {
			return nil, fmt.Errorf("failed to parse kubeconfig for cluster %s: %w", clusterName, err)
		}

		// Apply prefix to this config
		prefixedConfig := g.ApplyPrefix(config, clusterName)

		// Merge into the combined config
		for name, cluster := range prefixedConfig.Clusters {
			mergedConfig.Clusters[name] = cluster
		}

		for name, context := range prefixedConfig.Contexts {
			mergedConfig.Contexts[name] = context
		}

		for name, authInfo := range prefixedConfig.AuthInfos {
			mergedConfig.AuthInfos[name] = authInfo
		}
	}

	return mergedConfig, nil
}

// Serialize converts a kubeconfig to YAML format
func (g *Generator) Serialize(config *api.Config) ([]byte, error) {
	data, err := clientcmd.Write(*config)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize kubeconfig: %w", err)
	}
	return data, nil
}

// Generate creates a merged kubeconfig from multiple cluster kubeconfigs
func (g *Generator) Generate(clusterKubeconfigs map[string]string) ([]byte, error) {
	mergedConfig, err := g.MergeConfigs(clusterKubeconfigs)
	if err != nil {
		return nil, err
	}

	return g.Serialize(mergedConfig)
}
