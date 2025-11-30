// Package kubeconfig provides functionality for generating and merging kubeconfig files
package kubeconfig

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// AptakubeExtension represents the Aptakube extension data for a context
type AptakubeExtension struct {
	Tags []string `json:"tags,omitempty"`
}

// Generator handles kubeconfig generation and merging
type Generator struct {
	prefix string
	tags   map[string][]string // Map of context name to tags
}

// NewGenerator creates a new kubeconfig generator with the specified cluster name prefix
func NewGenerator(prefix string) *Generator {
	return &Generator{
		prefix: prefix,
		tags:   make(map[string][]string),
	}
}

// SetTags sets the Aptakube tags for a specific context
func (g *Generator) SetTags(contextName string, tags []string) {
	g.tags[contextName] = tags
}

// SetAllTags sets the same Aptakube tags for all contexts
func (g *Generator) SetAllTags(tags []string) {
	g.tags["*"] = tags
}

// getTagsForContext returns the tags for a specific context
func (g *Generator) getTagsForContext(contextName string) []string {
	if tags, ok := g.tags[contextName]; ok {
		return tags
	}
	if tags, ok := g.tags["*"]; ok {
		return tags
	}
	return nil
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
// It renames all entries to use clusterName as the base to ensure uniqueness when merging
func (g *Generator) ApplyPrefix(config *api.Config, clusterName string) *api.Config {
	// Always use clusterName as the base, with optional prefix
	newName := fmt.Sprintf("%s%s", g.prefix, clusterName)

	// Create new maps with renamed entries
	newClusters := make(map[string]*api.Cluster)
	newContexts := make(map[string]*api.Context)
	newAuthInfos := make(map[string]*api.AuthInfo)

	// Build a mapping from old cluster names to new names
	clusterNameMap := make(map[string]string)
	authNameMap := make(map[string]string)

	// Rename clusters - use clusterName as base for the first/only cluster
	i := 0
	for oldName, cluster := range config.Clusters {
		var mappedName string
		if i == 0 {
			mappedName = newName
		} else {
			mappedName = fmt.Sprintf("%s-%d", newName, i)
		}
		clusterNameMap[oldName] = mappedName
		newClusters[mappedName] = cluster
		i++
	}

	// Rename auth infos (users) - use clusterName as base
	i = 0
	for oldName, authInfo := range config.AuthInfos {
		var mappedName string
		if i == 0 {
			mappedName = newName
		} else {
			mappedName = fmt.Sprintf("%s-%d", newName, i)
		}
		authNameMap[oldName] = mappedName
		newAuthInfos[mappedName] = authInfo
		i++
	}

	// Rename and update contexts
	i = 0
	for _, context := range config.Contexts {
		var newContextName string
		if i == 0 {
			newContextName = newName
		} else {
			newContextName = fmt.Sprintf("%s-%d", newName, i)
		}

		// Create a copy of the context with updated references
		newContext := context.DeepCopy()

		// Update cluster reference using the mapping
		if mappedCluster, exists := clusterNameMap[context.Cluster]; exists {
			newContext.Cluster = mappedCluster
		}

		// Update auth info reference using the mapping
		if context.AuthInfo != "" {
			if mappedAuth, exists := authNameMap[context.AuthInfo]; exists {
				newContext.AuthInfo = mappedAuth
			}
		}

		newContexts[newContextName] = newContext
		i++
	}

	// Update current context to the new name
	newCurrentContext := newName

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
			// Add Aptakube extension if tags are specified
			tags := g.getTagsForContext(name)
			if len(tags) > 0 {
				if context.Extensions == nil {
					context.Extensions = make(map[string]runtime.Object)
				}
				context.Extensions["aptakube"] = &runtime.Unknown{
					Raw: g.buildAptakubeExtensionJSON(tags),
				}
			}
			mergedConfig.Contexts[name] = context
		}

		for name, authInfo := range prefixedConfig.AuthInfos {
			mergedConfig.AuthInfos[name] = authInfo
		}
	}

	return mergedConfig, nil
}

// buildAptakubeExtensionJSON builds the JSON for the Aptakube extension
func (g *Generator) buildAptakubeExtensionJSON(tags []string) []byte {
	// Build the JSON manually to ensure proper format
	result := `{"tags":[`
	for i, tag := range tags {
		if i > 0 {
			result += ","
		}
		result += fmt.Sprintf(`"%s"`, tag)
	}
	result += `]}`
	return []byte(result)
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
