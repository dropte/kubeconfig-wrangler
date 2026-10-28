// Package kubeconfig provides functionality for generating and merging kubeconfig files
package kubeconfig

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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

// ApplyPrefix renames all clusters, contexts and users in config to names
// based on prefix+clusterName so that configs can be merged without clashes.
//
// The result is deterministic and keeps entries identifiable:
//   - The "base" context is config.CurrentContext when it exists, otherwise
//     the lexicographically smallest context name. It, and the cluster and
//     user it references, are renamed to prefix+clusterName.
//   - Every other context, cluster and user is renamed to
//     base + "-" + suffix, where suffix is its original name with the base
//     context's original cluster name (plus a separator) stripped when that
//     is a prefix. Rancher's ACE contexts "prod", "prod-cp1", "prod-cp2"
//     therefore become "<p>prod", "<p>prod-cp1", "<p>prod-cp2".
//   - Name clashes inside one config are resolved with "-2", "-3", ... in
//     sorted order.
//
// CurrentContext of the result is the base context, or "" when the config
// has no contexts.
func (g *Generator) ApplyPrefix(config *api.Config, clusterName string) *api.Config {
	base := g.prefix + clusterName

	baseCtxName := ""
	if _, ok := config.Contexts[config.CurrentContext]; ok && config.CurrentContext != "" {
		baseCtxName = config.CurrentContext
	} else if names := sortedKeys(config.Contexts); len(names) > 0 {
		baseCtxName = names[0]
	}

	var baseCtx *api.Context
	stripPrefixes := []string{}
	if baseCtxName != "" {
		baseCtx = config.Contexts[baseCtxName]
		if baseCtx.Cluster != "" {
			stripPrefixes = append(stripPrefixes, baseCtx.Cluster)
		}
		if baseCtxName != baseCtx.Cluster {
			stripPrefixes = append(stripPrefixes, baseCtxName)
		}
	}

	derive := func(orig string) string {
		for _, p := range stripPrefixes {
			// Only strip at a separator so "production" is not cut by "prod".
			rest, ok := strings.CutPrefix(orig, p)
			if ok && rest != "" && strings.ContainsRune("-_.@:", rune(rest[0])) {
				if rest = strings.TrimLeft(rest, "-_.@:"); rest != "" {
					return base + "-" + rest
				}
			}
		}
		return base + "-" + orig
	}

	// rename assigns new names for all keys of one kind: baseKey (if any)
	// gets base, the rest get derived names made unique in sorted order.
	rename := func(keys []string, baseKey string) map[string]string {
		mapping := make(map[string]string, len(keys))
		used := map[string]bool{base: true}
		if baseKey != "" {
			mapping[baseKey] = base
		}
		for _, k := range keys {
			if _, done := mapping[k]; done {
				continue
			}
			name := uniqueName(derive(k), func(n string) bool { return used[n] })
			used[name] = true
			mapping[k] = name
		}
		return mapping
	}

	var baseCluster, baseUser string
	if baseCtx != nil {
		if _, ok := config.Clusters[baseCtx.Cluster]; ok {
			baseCluster = baseCtx.Cluster
		}
		if _, ok := config.AuthInfos[baseCtx.AuthInfo]; ok {
			baseUser = baseCtx.AuthInfo
		}
	}
	clusterMap := rename(sortedKeys(config.Clusters), baseCluster)
	authMap := rename(sortedKeys(config.AuthInfos), baseUser)
	contextMap := rename(sortedKeys(config.Contexts), baseCtxName)

	out := &api.Config{
		Kind:        config.Kind,
		APIVersion:  config.APIVersion,
		Clusters:    make(map[string]*api.Cluster, len(config.Clusters)),
		Contexts:    make(map[string]*api.Context, len(config.Contexts)),
		AuthInfos:   make(map[string]*api.AuthInfo, len(config.AuthInfos)),
		Preferences: config.Preferences,
		Extensions:  config.Extensions,
	}
	for old, c := range config.Clusters {
		out.Clusters[clusterMap[old]] = c.DeepCopy()
	}
	for old, a := range config.AuthInfos {
		out.AuthInfos[authMap[old]] = a.DeepCopy()
	}
	for old, c := range config.Contexts {
		nc := c.DeepCopy()
		if mapped, ok := clusterMap[c.Cluster]; ok {
			nc.Cluster = mapped
		}
		if mapped, ok := authMap[c.AuthInfo]; ok {
			nc.AuthInfo = mapped
		}
		out.Contexts[contextMap[old]] = nc
	}
	if baseCtxName != "" {
		out.CurrentContext = base
	}
	return out
}

// MergeConfigs merges multiple kubeconfig strings into a single config.
// The clusterKubeconfigs map has cluster names as keys and kubeconfig YAML
// strings as values. Inputs are processed in sorted key order, and a name
// that already exists in the merged config is never overwritten: it gets a
// "-2", "-3", ... suffix instead (with the references inside its contexts
// updated accordingly). CurrentContext is set to the base context of the
// first input (in sorted order) that has one.
func (g *Generator) MergeConfigs(clusterKubeconfigs map[string]string) (*api.Config, error) {
	merged, _, err := g.mergeConfigs(clusterKubeconfigs)
	return merged, err
}

func (g *Generator) mergeConfigs(clusterKubeconfigs map[string]string) (*api.Config, map[string][]string, error) {
	keys := sortedKeys(clusterKubeconfigs)

	prefixed := make([]*api.Config, len(keys))
	// Each source's base name is reserved up front so that a derived name
	// from one source (e.g. "prod-cp1") never takes the base name of another
	// source the user explicitly selected (a cluster called "prod-cp1").
	reserved := make(map[string]bool, len(keys))
	for i, key := range keys {
		config, err := g.ParseKubeconfig(clusterKubeconfigs[key])
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse kubeconfig for cluster %s: %w", key, err)
		}
		prefixed[i] = g.ApplyPrefix(config, key)
		reserved[g.prefix+key] = true
	}

	merged := api.NewConfig()
	contextNames := make(map[string][]string, len(keys))

	for i, key := range keys {
		pc := prefixed[i]
		base := g.prefix + key

		// alloc picks the final name for an entry of one kind.
		alloc := func(name string, exists func(string) bool) string {
			if name == base && !exists(name) {
				return name
			}
			return uniqueName(name, func(n string) bool { return exists(n) || (reserved[n] && n != base) })
		}

		clusterMap := make(map[string]string, len(pc.Clusters))
		for _, name := range sortedKeys(pc.Clusters) {
			final := alloc(name, func(n string) bool { _, ok := merged.Clusters[n]; return ok })
			clusterMap[name] = final
			merged.Clusters[final] = pc.Clusters[name]
		}
		authMap := make(map[string]string, len(pc.AuthInfos))
		for _, name := range sortedKeys(pc.AuthInfos) {
			final := alloc(name, func(n string) bool { _, ok := merged.AuthInfos[n]; return ok })
			authMap[name] = final
			merged.AuthInfos[final] = pc.AuthInfos[name]
		}

		var names []string
		for _, name := range sortedKeys(pc.Contexts) {
			final := alloc(name, func(n string) bool { _, ok := merged.Contexts[n]; return ok })
			ctx := pc.Contexts[name]
			if mapped, ok := clusterMap[ctx.Cluster]; ok {
				ctx.Cluster = mapped
			}
			if mapped, ok := authMap[ctx.AuthInfo]; ok {
				ctx.AuthInfo = mapped
			}
			if tags := g.getTagsForContext(final); len(tags) > 0 {
				if ctx.Extensions == nil {
					ctx.Extensions = make(map[string]runtime.Object)
				}
				ctx.Extensions["aptakube"] = &runtime.Unknown{
					Raw:         buildAptakubeExtensionJSON(tags),
					ContentType: runtime.ContentTypeJSON,
				}
			}
			merged.Contexts[final] = ctx

			if name == pc.CurrentContext {
				// Base context first, so callers can rely on names[0].
				names = append([]string{final}, names...)
				if merged.CurrentContext == "" {
					merged.CurrentContext = final
				}
			} else {
				names = append(names, final)
			}
		}
		contextNames[key] = names
	}

	return merged, contextNames, nil
}

// buildAptakubeExtensionJSON builds the JSON for the Aptakube extension.
func buildAptakubeExtensionJSON(tags []string) []byte {
	// Marshalling a struct of strings cannot fail.
	b, _ := json.Marshal(AptakubeExtension{Tags: tags})
	return b
}

// uniqueName returns name if it is not taken, otherwise name-2, name-3, ...
func uniqueName(name string, taken func(string) bool) string {
	if !taken(name) {
		return name
	}
	for i := 2; ; i++ {
		if candidate := fmt.Sprintf("%s-%d", name, i); !taken(candidate) {
			return candidate
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
	data, _, err := g.GenerateWithNames(clusterKubeconfigs)
	return data, err
}

// GenerateWithNames is Generate that also reports, per input key, the final
// context names written for it. The base context (the one named after the
// key) comes first in each slice.
func (g *Generator) GenerateWithNames(clusterKubeconfigs map[string]string) ([]byte, map[string][]string, error) {
	merged, names, err := g.mergeConfigs(clusterKubeconfigs)
	if err != nil {
		return nil, nil, err
	}
	data, err := g.Serialize(merged)
	if err != nil {
		return nil, nil, err
	}
	return data, names, nil
}
