package profile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	appName      = "kubeconfig-wrangler"
	profilesFile = "profiles.json"
)

// Store handles persistent storage of profiles
type Store struct {
	mu        sync.RWMutex
	path      string
	encryptor *Encryptor
	profiles  map[string]*Profile
}

// storeData represents the on-disk format of the profile store
type storeData struct {
	Version  int        `json:"version"`
	Profiles []*Profile `json:"profiles"`
}

// NewStore creates a new profile store
func NewStore() (*Store, error) {
	path, err := getStorePath()
	if err != nil {
		return nil, err
	}

	encryptor, err := NewEncryptor()
	if err != nil {
		return nil, fmt.Errorf("failed to create encryptor: %w", err)
	}

	store := &Store{
		path:      path,
		encryptor: encryptor,
		profiles:  make(map[string]*Profile),
	}

	if err := store.load(); err != nil {
		return nil, err
	}

	return store, nil
}

// NewStoreWithPath creates a new profile store with a custom path (for testing)
func NewStoreWithPath(path string) (*Store, error) {
	encryptor, err := NewEncryptor()
	if err != nil {
		return nil, fmt.Errorf("failed to create encryptor: %w", err)
	}

	store := &Store{
		path:      path,
		encryptor: encryptor,
		profiles:  make(map[string]*Profile),
	}

	if err := store.load(); err != nil {
		return nil, err
	}

	return store, nil
}

// getStorePath returns the platform-specific path for storing profiles
func getStorePath() (string, error) {
	var configDir string

	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		configDir = filepath.Join(home, "Library", "Application Support", appName)
	case "linux":
		// Use XDG_CONFIG_HOME if set, otherwise ~/.config
		xdgConfig := os.Getenv("XDG_CONFIG_HOME")
		if xdgConfig != "" {
			configDir = filepath.Join(xdgConfig, appName)
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("failed to get home directory: %w", err)
			}
			configDir = filepath.Join(home, ".config", appName)
		}
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", fmt.Errorf("APPDATA environment variable not set")
		}
		configDir = filepath.Join(appData, appName)
	default:
		return "", fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}

	// Ensure directory exists
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return "", fmt.Errorf("failed to create config directory: %w", err)
	}

	return filepath.Join(configDir, profilesFile), nil
}

// load reads profiles from disk
func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		// No profiles yet, that's fine
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read profiles file: %w", err)
	}

	var stored storeData
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("failed to parse profiles file: %w", err)
	}

	// Decrypt and store profiles
	for _, p := range stored.Profiles {
		decrypted, err := s.encryptor.DecryptProfile(p)
		if err != nil {
			// Log warning but continue loading other profiles
			fmt.Fprintf(os.Stderr, "Warning: failed to decrypt profile %s: %v\n", p.Name, err)
			continue
		}
		s.profiles[decrypted.ID] = decrypted
	}

	return nil
}

// save writes profiles to disk
func (s *Store) save() error {
	profiles := make([]*Profile, 0, len(s.profiles))
	for _, p := range s.profiles {
		encrypted, err := s.encryptor.EncryptProfile(p)
		if err != nil {
			return fmt.Errorf("failed to encrypt profile %s: %w", p.Name, err)
		}
		profiles = append(profiles, encrypted)
	}

	stored := storeData{
		Version:  1,
		Profiles: profiles,
	}

	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal profiles: %w", err)
	}

	// Write to temp file first, then rename for atomic write
	tempPath := s.path + ".tmp"
	if err := os.WriteFile(tempPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write profiles file: %w", err)
	}

	if err := os.Rename(tempPath, s.path); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("failed to rename profiles file: %w", err)
	}

	return nil
}

// List returns all profiles
func (s *Store) List() []*Profile {
	s.mu.RLock()
	defer s.mu.RUnlock()

	profiles := make([]*Profile, 0, len(s.profiles))
	for _, p := range s.profiles {
		profiles = append(profiles, p)
	}
	return profiles
}

// Get retrieves a profile by ID
func (s *Store) Get(id string) (*Profile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, exists := s.profiles[id]
	if !exists {
		return nil, fmt.Errorf("profile not found: %s", id)
	}
	return p, nil
}

// Create adds a new profile
func (s *Store) Create(req *ProfileCreateRequest) (*Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := uuid.New().String()
	profile := req.ToProfile(id)

	if err := profile.Validate(); err != nil {
		return nil, err
	}

	s.profiles[id] = profile

	if err := s.save(); err != nil {
		delete(s.profiles, id)
		return nil, err
	}

	return profile, nil
}

// Update modifies an existing profile
func (s *Store) Update(id string, req *ProfileCreateRequest) (*Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, exists := s.profiles[id]
	if !exists {
		return nil, fmt.Errorf("profile not found: %s", id)
	}

	profile := req.ToProfile(id)
	profile.CreatedAt = existing.CreatedAt

	if err := profile.Validate(); err != nil {
		return nil, err
	}

	s.profiles[id] = profile

	if err := s.save(); err != nil {
		s.profiles[id] = existing
		return nil, err
	}

	return profile, nil
}

// Delete removes a profile
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, exists := s.profiles[id]
	if !exists {
		return fmt.Errorf("profile not found: %s", id)
	}

	delete(s.profiles, id)

	if err := s.save(); err != nil {
		s.profiles[id] = existing
		return err
	}

	return nil
}

// Path returns the storage path
func (s *Store) Path() string {
	return s.path
}

// SetClusterAlias sets or removes a cluster alias for a profile
func (s *Store) SetClusterAlias(profileID, clusterID, alias string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	profile, exists := s.profiles[profileID]
	if !exists {
		return fmt.Errorf("profile not found: %s", profileID)
	}

	if profile.ClusterAliases == nil {
		profile.ClusterAliases = make(map[string]string)
	}

	if alias == "" {
		delete(profile.ClusterAliases, clusterID)
	} else {
		profile.ClusterAliases[clusterID] = alias
	}

	profile.UpdatedAt = time.Now()

	return s.save()
}

// GetClusterAlias returns the alias for a cluster in a profile, or empty string if none
func (s *Store) GetClusterAlias(profileID, clusterID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	profile, exists := s.profiles[profileID]
	if !exists || profile.ClusterAliases == nil {
		return ""
	}

	return profile.ClusterAliases[clusterID]
}

// SetExportedContext records that a cluster was merged to kubeconfig with the given context name
func (s *Store) SetExportedContext(profileID, clusterID, contextName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	profile, exists := s.profiles[profileID]
	if !exists {
		return fmt.Errorf("profile not found: %s", profileID)
	}

	if profile.ExportedContexts == nil {
		profile.ExportedContexts = make(map[string]string)
	}

	if contextName == "" {
		delete(profile.ExportedContexts, clusterID)
	} else {
		profile.ExportedContexts[clusterID] = contextName
	}

	profile.UpdatedAt = time.Now()

	return s.save()
}

// GetExportedContext returns the exported context name for a cluster, or empty string if not exported
func (s *Store) GetExportedContext(profileID, clusterID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	profile, exists := s.profiles[profileID]
	if !exists || profile.ExportedContexts == nil {
		return ""
	}

	return profile.ExportedContexts[clusterID]
}

// GetAllExportedContexts returns all exported context names across all profiles
func (s *Store) GetAllExportedContexts() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string]string)
	for _, profile := range s.profiles {
		if profile.ExportedContexts != nil {
			for clusterID, contextName := range profile.ExportedContexts {
				// Key by profileID:clusterID to avoid collisions
				key := profile.ID + ":" + clusterID
				result[key] = contextName
			}
		}
	}
	return result
}
