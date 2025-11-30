// Package context provides utilities for reading and modifying kubeconfig contexts
package context

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// ContextInfo represents information about a kubeconfig context
type ContextInfo struct {
	Name      string `json:"name"`
	Cluster   string `json:"cluster"`
	User      string `json:"user"`
	Namespace string `json:"namespace,omitempty"`
	IsCurrent bool   `json:"isCurrent"`
}

// Switcher handles reading and modifying kubeconfig contexts
type Switcher struct {
	kubeconfigPath string
}

// NewSwitcher creates a new context switcher
func NewSwitcher() *Switcher {
	return &Switcher{
		kubeconfigPath: GetDefaultKubeconfigPath(),
	}
}

// NewSwitcherWithPath creates a new context switcher with a custom path
func NewSwitcherWithPath(path string) *Switcher {
	return &Switcher{
		kubeconfigPath: path,
	}
}

// GetDefaultKubeconfigPath returns the default kubeconfig path
func GetDefaultKubeconfigPath() string {
	// Check KUBECONFIG environment variable first
	if envPath := os.Getenv("KUBECONFIG"); envPath != "" {
		// KUBECONFIG can contain multiple paths, use the first one
		paths := filepath.SplitList(envPath)
		if len(paths) > 0 {
			return paths[0]
		}
	}

	// Default to ~/.kube/config
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(homeDir, ".kube", "config")
}

// Path returns the kubeconfig path being used
func (s *Switcher) Path() string {
	return s.kubeconfigPath
}

// ListContexts returns all contexts from the kubeconfig
func (s *Switcher) ListContexts() ([]ContextInfo, error) {
	cfg, err := s.loadConfig()
	if err != nil {
		return nil, err
	}

	contexts := make([]ContextInfo, 0, len(cfg.Contexts))
	for name, ctx := range cfg.Contexts {
		info := ContextInfo{
			Name:      name,
			Cluster:   ctx.Cluster,
			User:      ctx.AuthInfo,
			Namespace: ctx.Namespace,
			IsCurrent: name == cfg.CurrentContext,
		}
		contexts = append(contexts, info)
	}

	return contexts, nil
}

// GetCurrentContext returns the current context name
func (s *Switcher) GetCurrentContext() (string, error) {
	cfg, err := s.loadConfig()
	if err != nil {
		return "", err
	}

	return cfg.CurrentContext, nil
}

// SetCurrentContext sets the current context
func (s *Switcher) SetCurrentContext(contextName string) error {
	cfg, err := s.loadConfig()
	if err != nil {
		return err
	}

	// Verify the context exists
	if _, exists := cfg.Contexts[contextName]; !exists {
		return fmt.Errorf("context %q not found", contextName)
	}

	cfg.CurrentContext = contextName

	return s.saveConfig(cfg)
}

// AddContext adds a new context to the kubeconfig
func (s *Switcher) AddContext(name string, ctx *clientcmdapi.Context) error {
	cfg, err := s.loadConfig()
	if err != nil {
		// If file doesn't exist, create a new config
		if os.IsNotExist(err) {
			cfg = clientcmdapi.NewConfig()
		} else {
			return err
		}
	}

	cfg.Contexts[name] = ctx
	return s.saveConfig(cfg)
}

// RemoveContext removes a context from the kubeconfig
func (s *Switcher) RemoveContext(name string) error {
	cfg, err := s.loadConfig()
	if err != nil {
		return err
	}

	if _, exists := cfg.Contexts[name]; !exists {
		return fmt.Errorf("context %q not found", name)
	}

	delete(cfg.Contexts, name)

	// If we removed the current context, clear it
	if cfg.CurrentContext == name {
		cfg.CurrentContext = ""
	}

	return s.saveConfig(cfg)
}

// DeleteContextWithCleanup removes a context and optionally cleans up orphaned clusters/users
func (s *Switcher) DeleteContextWithCleanup(name string, cleanupOrphans bool) error {
	cfg, err := s.loadConfig()
	if err != nil {
		return err
	}

	ctx, exists := cfg.Contexts[name]
	if !exists {
		return fmt.Errorf("context %q not found", name)
	}

	clusterName := ctx.Cluster
	userName := ctx.AuthInfo

	delete(cfg.Contexts, name)

	// If we removed the current context, clear it
	if cfg.CurrentContext == name {
		cfg.CurrentContext = ""
	}

	if cleanupOrphans {
		// Check if the cluster is still used by other contexts
		clusterInUse := false
		userInUse := false
		for _, c := range cfg.Contexts {
			if c.Cluster == clusterName {
				clusterInUse = true
			}
			if c.AuthInfo == userName {
				userInUse = true
			}
		}

		// Delete orphaned cluster
		if !clusterInUse && clusterName != "" {
			delete(cfg.Clusters, clusterName)
		}

		// Delete orphaned user
		if !userInUse && userName != "" {
			delete(cfg.AuthInfos, userName)
		}
	}

	return s.saveConfig(cfg)
}

// RenameContext renames a context
func (s *Switcher) RenameContext(oldName, newName string) error {
	if oldName == newName {
		return nil
	}

	cfg, err := s.loadConfig()
	if err != nil {
		return err
	}

	ctx, exists := cfg.Contexts[oldName]
	if !exists {
		return fmt.Errorf("context %q not found", oldName)
	}

	if _, exists := cfg.Contexts[newName]; exists {
		return fmt.Errorf("context %q already exists", newName)
	}

	// Add context with new name
	cfg.Contexts[newName] = ctx

	// Remove old context
	delete(cfg.Contexts, oldName)

	// Update current context if it was the renamed one
	if cfg.CurrentContext == oldName {
		cfg.CurrentContext = newName
	}

	return s.saveConfig(cfg)
}

// MergeKubeconfig merges a kubeconfig string into the existing kubeconfig
func (s *Switcher) MergeKubeconfig(kubeconfigContent string) error {
	// Parse the input kubeconfig
	newCfg, err := clientcmd.Load([]byte(kubeconfigContent))
	if err != nil {
		return fmt.Errorf("failed to parse kubeconfig: %w", err)
	}

	// Load existing config
	existingCfg, err := s.loadConfig()
	if err != nil {
		if os.IsNotExist(err) {
			// No existing config, just save the new one
			return s.saveConfig(newCfg)
		}
		return err
	}

	// Merge clusters
	for name, cluster := range newCfg.Clusters {
		existingCfg.Clusters[name] = cluster
	}

	// Merge users
	for name, user := range newCfg.AuthInfos {
		existingCfg.AuthInfos[name] = user
	}

	// Merge contexts
	for name, ctx := range newCfg.Contexts {
		existingCfg.Contexts[name] = ctx
	}

	// Update current context if the new config has one
	if newCfg.CurrentContext != "" {
		existingCfg.CurrentContext = newCfg.CurrentContext
	}

	return s.saveConfig(existingCfg)
}

// loadConfig loads the kubeconfig from disk
func (s *Switcher) loadConfig() (*clientcmdapi.Config, error) {
	if s.kubeconfigPath == "" {
		return nil, fmt.Errorf("kubeconfig path not set")
	}

	// Check if file exists
	if _, err := os.Stat(s.kubeconfigPath); os.IsNotExist(err) {
		return nil, err
	}

	cfg, err := clientcmd.LoadFromFile(s.kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	return cfg, nil
}

// saveConfig saves the kubeconfig to disk
func (s *Switcher) saveConfig(cfg *clientcmdapi.Config) error {
	if s.kubeconfigPath == "" {
		return fmt.Errorf("kubeconfig path not set")
	}

	// Ensure the directory exists
	dir := filepath.Dir(s.kubeconfigPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create kubeconfig directory: %w", err)
	}

	if err := clientcmd.WriteToFile(*cfg, s.kubeconfigPath); err != nil {
		return fmt.Errorf("failed to write kubeconfig: %w", err)
	}

	return nil
}

// Exists checks if the kubeconfig file exists
func (s *Switcher) Exists() bool {
	if s.kubeconfigPath == "" {
		return false
	}
	_, err := os.Stat(s.kubeconfigPath)
	return err == nil
}
