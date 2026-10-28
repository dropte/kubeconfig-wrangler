// Package context provides utilities for reading and modifying kubeconfig contexts
package context

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"syscall"

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
	// otherPaths are the remaining files of a multi-file KUBECONFIG. They
	// are never written, but their contexts count as references when
	// orphaned clusters and users are cleaned up.
	otherPaths []string
}

// NewSwitcher creates a new context switcher for the default kubeconfig
// (the first KUBECONFIG entry, or ~/.kube/config).
func NewSwitcher() *Switcher {
	return &Switcher{
		kubeconfigPath: GetDefaultKubeconfigPath(),
		otherPaths:     otherKubeconfigPaths(),
	}
}

// otherKubeconfigPaths returns the KUBECONFIG entries after the first one,
// skipping empty entries and duplicates of earlier ones.
func otherKubeconfigPaths() []string {
	var paths []string
	seen := map[string]bool{}
	for _, p := range filepath.SplitList(os.Getenv("KUBECONFIG")) {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	if len(paths) < 2 {
		return nil
	}
	return paths[1:]
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
		// KUBECONFIG can contain multiple paths; changes go to the first
		// one (other entries are only read, see DeleteContextWithCleanup).
		for _, p := range filepath.SplitList(envPath) {
			if p != "" {
				return p
			}
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

// DeleteContextWithCleanup removes a context and optionally cleans up
// orphaned clusters/users. A cluster or user is only removed when no
// remaining context references it, including contexts in the other files of
// a multi-file KUBECONFIG; only the first file is written.
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
		// Check if the cluster is still used by other contexts, in this
		// file or in any other KUBECONFIG file.
		contexts := []map[string]*clientcmdapi.Context{cfg.Contexts}
		for _, other := range s.otherPaths {
			otherCfg, err := clientcmd.LoadFromFile(other)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("cannot check %s for references before removing orphaned entries: %w", other, err)
			}
			contexts = append(contexts, otherCfg.Contexts)
		}
		clusterInUse := false
		userInUse := false
		for _, m := range contexts {
			for _, c := range m {
				if c.Cluster == clusterName {
					clusterInUse = true
				}
				if c.AuthInfo == userName {
					userInUse = true
				}
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

// MergeKubeconfig merges a kubeconfig string into the existing kubeconfig.
// See MergeKubeconfigResult.
func (s *Switcher) MergeKubeconfig(kubeconfigContent string) error {
	_, err := s.MergeKubeconfigResult(kubeconfigContent)
	return err
}

// MergeKubeconfigReport merges a kubeconfig string into the existing
// kubeconfig and returns the context names written, split into those that
// were new and those that replaced an existing context of the same name.
// See MergeKubeconfigResult for the full report, including clusters and
// users.
func (s *Switcher) MergeKubeconfigReport(kubeconfigContent string) (added []string, replaced []string, err error) {
	res, err := s.MergeKubeconfigResult(kubeconfigContent)
	if err != nil {
		return nil, nil, err
	}
	return res.Added, res.Replaced, nil
}

// MergeResult describes what a merge changed.
type MergeResult struct {
	// Added and Replaced are context names that were new or replaced an
	// existing context of the same name.
	Added    []string `json:"added"`
	Replaced []string `json:"replaced"`
	// ReplacedClusters and ReplacedUsers are existing clusters and users
	// that were overwritten with different content. Identical entries
	// (a routine refresh) are not listed.
	ReplacedClusters []string `json:"replacedClusters,omitempty"`
	ReplacedUsers    []string `json:"replacedUsers,omitempty"`
	// RenamedClusters and RenamedUsers map an incoming cluster or user name
	// to the name it was written under, because an existing entry of the
	// same name with different content is still used by a context outside
	// this merge. The merged contexts point at the new name.
	RenamedClusters map[string]string `json:"renamedClusters,omitempty"`
	RenamedUsers    map[string]string `json:"renamedUsers,omitempty"`
}

// MergeKubeconfigResult merges a kubeconfig string into the existing
// kubeconfig. The existing current-context is kept; the merged config's
// current-context is only adopted when the file has none. When a context is
// replaced, its namespace and extensions are kept if the incoming context
// leaves them empty.
//
// Clusters and users are compared with existing entries of the same name.
// A differing entry that only contexts being merged (or none) reference is
// overwritten and reported in ReplacedClusters/ReplacedUsers. A differing
// entry that some other existing context still references is left alone;
// the incoming entry is written under a unique name (name-2, name-3, ...)
// and the merged contexts are pointed at it, so unrelated contexts are never
// silently repointed.
func (s *Switcher) MergeKubeconfigResult(kubeconfigContent string) (*MergeResult, error) {
	newCfg, err := clientcmd.Load([]byte(kubeconfigContent))
	if err != nil {
		return nil, fmt.Errorf("failed to parse kubeconfig: %w", err)
	}

	existingCfg, err := s.loadConfig()
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		existingCfg = clientcmdapi.NewConfig()
	}

	res := &MergeResult{}

	// References from existing contexts that this merge does not replace.
	outsideClusters := map[string]bool{}
	outsideUsers := map[string]bool{}
	for name, ctx := range existingCfg.Contexts {
		if _, merged := newCfg.Contexts[name]; merged {
			continue
		}
		outsideClusters[ctx.Cluster] = true
		outsideUsers[ctx.AuthInfo] = true
	}

	res.RenamedClusters, res.ReplacedClusters = mergeEntries(
		existingCfg.Clusters, newCfg.Clusters, outsideClusters, clustersEqual)
	res.RenamedUsers, res.ReplacedUsers = mergeEntries(
		existingCfg.AuthInfos, newCfg.AuthInfos, outsideUsers, usersEqual)

	for name, ctx := range newCfg.Contexts {
		if renamed, ok := res.RenamedClusters[ctx.Cluster]; ok {
			ctx.Cluster = renamed
		}
		if renamed, ok := res.RenamedUsers[ctx.AuthInfo]; ok {
			ctx.AuthInfo = renamed
		}
		if old, ok := existingCfg.Contexts[name]; ok {
			if ctx.Namespace == "" {
				ctx.Namespace = old.Namespace
			}
			if len(ctx.Extensions) == 0 {
				ctx.Extensions = old.Extensions
			}
			res.Replaced = append(res.Replaced, name)
		} else {
			res.Added = append(res.Added, name)
		}
		existingCfg.Contexts[name] = ctx
	}
	sort.Strings(res.Added)
	sort.Strings(res.Replaced)
	if len(res.RenamedClusters) == 0 {
		res.RenamedClusters = nil
	}
	if len(res.RenamedUsers) == 0 {
		res.RenamedUsers = nil
	}

	if existingCfg.CurrentContext == "" && newCfg.CurrentContext != "" {
		existingCfg.CurrentContext = newCfg.CurrentContext
	}

	if err := s.saveConfig(existingCfg); err != nil {
		return nil, err
	}
	return res, nil
}

// mergeEntries copies incoming cluster or user entries into existing.
// It returns the incoming names written under a different name (because an
// existing, different entry is still referenced from outside the merge) and
// the sorted names of existing entries overwritten with different content.
func mergeEntries[T any](existing, incoming map[string]*T, outside map[string]bool, equal func(a, b *T) bool) (renamed map[string]string, replaced []string) {
	renamed = map[string]string{}
	names := make([]string, 0, len(incoming))
	for name := range incoming {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		entry := incoming[name]
		old, exists := existing[name]
		switch {
		case !exists || equal(old, entry):
			existing[name] = entry
		case !outside[name]:
			existing[name] = entry
			replaced = append(replaced, name)
		default:
			target := uniqueEntryName(name, existing, incoming, outside, entry, equal)
			if old, ok := existing[target]; ok && !equal(old, entry) {
				replaced = append(replaced, target)
			}
			existing[target] = entry
			renamed[name] = target
		}
	}
	sort.Strings(replaced)
	return renamed, replaced
}

// uniqueEntryName returns the first of name-2, name-3, ... that is not an
// incoming name and is either free, already identical to entry, or not
// referenced from outside the merge (typically the entry an earlier merge of
// the same source wrote).
func uniqueEntryName[T any](name string, existing, incoming map[string]*T, outside map[string]bool, entry *T, equal func(a, b *T) bool) string {
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", name, i)
		if _, clash := incoming[candidate]; clash {
			continue
		}
		old, exists := existing[candidate]
		if !exists || equal(old, entry) || !outside[candidate] {
			return candidate
		}
	}
}

// clustersEqual compares two clusters ignoring where they were loaded from.
func clustersEqual(a, b *clientcmdapi.Cluster) bool {
	if a == nil || b == nil {
		return a == b
	}
	ac, bc := *a, *b
	ac.LocationOfOrigin, bc.LocationOfOrigin = "", ""
	if len(ac.Extensions) == 0 {
		ac.Extensions = nil
	}
	if len(bc.Extensions) == 0 {
		bc.Extensions = nil
	}
	return reflect.DeepEqual(ac, bc)
}

// usersEqual compares two users ignoring where they were loaded from.
func usersEqual(a, b *clientcmdapi.AuthInfo) bool {
	if a == nil || b == nil {
		return a == b
	}
	ac, bc := *a, *b
	ac.LocationOfOrigin, bc.LocationOfOrigin = "", ""
	if len(ac.Extensions) == 0 {
		ac.Extensions = nil
	}
	if len(bc.Extensions) == 0 {
		bc.Extensions = nil
	}
	return reflect.DeepEqual(ac, bc)
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

// backedUp records kubeconfig files already copied to <path>.bak by this
// process, so the backup holds the state from before the first change.
var (
	backupMu sync.Mutex
	backedUp = map[string]bool{}
)

// saveConfig writes the kubeconfig atomically: the data goes to a 0600 temp
// file in the same directory, is fsynced and renamed over the target. A
// symlinked kubeconfig is resolved first so the link itself stays in place.
// Before the first change in this process the existing file is copied to
// <path>.bak.
func (s *Switcher) saveConfig(cfg *clientcmdapi.Config) error {
	if s.kubeconfigPath == "" {
		return fmt.Errorf("kubeconfig path not set")
	}

	target := s.kubeconfigPath
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}

	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return fmt.Errorf("failed to create kubeconfig directory: %w", err)
	}

	data, err := clientcmd.Write(*cfg)
	if err != nil {
		return fmt.Errorf("failed to serialize kubeconfig: %w", err)
	}

	if err := backupOnce(target); err != nil {
		return err
	}

	if err := replaceKubeconfig(target, data); err != nil {
		return fmt.Errorf("failed to write kubeconfig: %w", err)
	}
	return nil
}

// renameFile is os.Rename; tests replace it to simulate a failing rename.
var renameFile = os.Rename

// replaceKubeconfig writes data to path. Normally the file is replaced
// atomically (temp file + rename), keeping the existing mode when it is no
// looser than 0600. The file is rewritten in place instead, keeping its
// inode, owner and mode, when it has several hard links, or when the rename
// fails with EBUSY or EXDEV, as it does for a kubeconfig bind-mounted as a
// single file into a container (devcontainers, Docker, WSL). The caller has
// already written the .bak copy.
func replaceKubeconfig(path string, data []byte) error {
	mode := os.FileMode(0600)
	if info, err := os.Stat(path); err == nil {
		if hasMultipleLinks(info) {
			return writeInPlace(path, data)
		}
		if perm := info.Mode().Perm(); perm&^0600 == 0 {
			mode = perm
		}
	}
	err := writeFileAtomicMode(path, data, mode)
	if err != nil && (errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.EXDEV)) {
		return writeInPlace(path, data)
	}
	return err
}

// writeInPlace truncates and rewrites an existing file, keeping its inode,
// owner and mode.
func writeInPlace(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// backupOnce copies path to path.bak the first time it is called for path in
// this process. A missing file needs no backup.
func backupOnce(path string) error {
	backupMu.Lock()
	defer backupMu.Unlock()

	if backedUp[path] {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read kubeconfig for backup: %w", err)
	}
	if err == nil {
		if err := writeFileAtomic(path+".bak", data); err != nil {
			return fmt.Errorf("failed to back up kubeconfig: %w", err)
		}
	}
	backedUp[path] = true
	return nil
}

// writeFileAtomic writes data to a 0600 temp file in path's directory, syncs
// it and renames it over path.
func writeFileAtomic(path string, data []byte) error {
	return writeFileAtomicMode(path, data, 0600)
}

// writeFileAtomicMode is writeFileAtomic with the given file mode.
func writeFileAtomicMode(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename

	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return renameFile(tmpPath, path)
}

// Exists checks if the kubeconfig file exists
func (s *Switcher) Exists() bool {
	if s.kubeconfigPath == "" {
		return false
	}
	_, err := os.Stat(s.kubeconfigPath)
	return err == nil
}

// GetLocalClusterNames returns a set of all cluster names and context names from the local kubeconfig.
// This is useful for checking if a cluster already exists in the local config.
// Returns both cluster names (from the clusters section) and context names to maximize match chances.
func (s *Switcher) GetLocalClusterNames() (map[string]bool, error) {
	names := make(map[string]bool)

	cfg, err := s.loadConfig()
	if err != nil {
		// If kubeconfig doesn't exist, return empty set
		if os.IsNotExist(err) {
			return names, nil
		}
		return names, err
	}

	// Add all cluster names
	for clusterName := range cfg.Clusters {
		names[clusterName] = true
	}

	// Add all context names (sometimes context name matches cluster better than cluster name)
	for contextName := range cfg.Contexts {
		names[contextName] = true
	}

	return names, nil
}
