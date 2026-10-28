package profile

import (
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	appName      = "kubeconfig-wrangler"
	profilesFile = "profiles.json"

	// configDirEnv overrides the platform config directory.
	configDirEnv = "KUBECONFIG_WRANGLER_CONFIG_DIR"
)

// Store handles persistent storage of profiles.
//
// Several processes may share the file (for example the desktop app and a
// separate "serve"). Every mutation takes an advisory lock on profiles.lock,
// re-reads profiles.json under it, applies only its own change and rewrites
// the file atomically, so concurrent writers do not lose each other's
// updates. Reads (List, Get, ...) use the in-memory copy from the last load
// or mutation and may briefly lag behind another process.
type Store struct {
	mu        sync.RWMutex
	path      string
	lockPath  string
	encryptor *Encryptor
	profiles  map[string]*Profile // decrypted, readable profiles

	// unreadable holds profiles whose secrets could not be decrypted, exactly
	// as they were read from disk. They are written back verbatim so a
	// missing or wrong key never destroys data.
	unreadable map[string]*Profile
	warnings   []string
	// backupPending is set when load skipped profiles; the next save first
	// copies the on-disk file to a timestamped backup. backupDone records
	// that this store already made that backup.
	backupPending bool
	backupDone    bool
}

// storeData represents the on-disk format of the profile store
type storeData struct {
	Version  int        `json:"version"`
	Profiles []*Profile `json:"profiles"`
}

// NewStore creates a new profile store in ConfigDir, keyed from the OS
// keychain (or the fallback key file).
func NewStore() (*Store, error) {
	dir, err := ConfigDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, profilesFile)

	key, err := getOrCreateKeyLocked(dir, path)
	if err != nil {
		return nil, fmt.Errorf("failed to create encryptor: %w", err)
	}
	return NewStoreWithOptions(path, &Encryptor{key: key})
}

// NewStoreWithPath creates a new profile store with a custom path, keyed like
// NewStore. Tests should use NewStoreWithOptions instead.
func NewStoreWithPath(path string) (*Store, error) {
	dir, err := ConfigDir()
	if err != nil {
		return nil, err
	}
	key, err := getOrCreateKeyLocked(dir, path)
	if err != nil {
		return nil, fmt.Errorf("failed to create encryptor: %w", err)
	}
	return NewStoreWithOptions(path, &Encryptor{key: key})
}

// NewStoreWithOptions creates a store at path using enc. It performs no
// keychain or environment lookups.
func NewStoreWithOptions(path string, enc *Encryptor) (*Store, error) {
	if enc == nil {
		return nil, fmt.Errorf("encryptor is required")
	}
	store := &Store{
		path:       path,
		lockPath:   lockPathFor(path),
		encryptor:  enc,
		profiles:   make(map[string]*Profile),
		unreadable: make(map[string]*Profile),
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

// ConfigDir returns (and creates) the platform-specific configuration
// directory. KUBECONFIG_WRANGLER_CONFIG_DIR overrides it.
func ConfigDir() (string, error) {
	configDir := os.Getenv(configDirEnv)

	if configDir == "" {
		switch runtime.GOOS {
		case "darwin":
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("failed to get home directory: %w", err)
			}
			configDir = filepath.Join(home, "Library", "Application Support", appName)
		case "linux":
			// Use XDG_CONFIG_HOME if set, otherwise ~/.config
			if xdgConfig := os.Getenv("XDG_CONFIG_HOME"); xdgConfig != "" {
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
	}

	if err := os.MkdirAll(configDir, 0700); err != nil {
		return "", fmt.Errorf("failed to create config directory: %w", err)
	}
	return configDir, nil
}

// load reads profiles from disk, replacing the in-memory state. Callers
// other than the constructor must hold the write lock.
func (s *Store) load() error {
	profiles := make(map[string]*Profile)
	unreadable := make(map[string]*Profile)
	var warnings []string

	data, err := os.ReadFile(s.path)
	switch {
	case os.IsNotExist(err):
		// No profiles yet, that's fine
	case err != nil:
		return fmt.Errorf("failed to read profiles file: %w", err)
	default:
		var stored storeData
		if err := json.Unmarshal(data, &stored); err != nil {
			return fmt.Errorf("failed to parse profiles file: %w", err)
		}
		for _, p := range stored.Profiles {
			if p == nil {
				continue
			}
			decrypted, err := s.encryptor.DecryptProfile(p)
			if err != nil {
				msg := fmt.Sprintf("profile %q (%s) could not be decrypted: %v", p.Name, p.ID, err)
				if _, known := s.unreadable[p.ID]; !known {
					log.Printf("Warning: %s", msg)
				}
				warnings = append(warnings, msg)
				unreadable[p.ID] = p
				continue
			}
			profiles[decrypted.ID] = decrypted
		}
	}

	s.profiles, s.unreadable, s.warnings = profiles, unreadable, warnings
	s.backupPending = len(unreadable) > 0 && !s.backupDone
	return nil
}

// mutate runs fn as one cross-process read-modify-write: it takes the
// advisory file lock, re-reads profiles.json so changes made by other
// processes are not overwritten, then runs fn, which applies this
// operation's change and saves. Callers must hold the write lock.
func (s *Store) mutate(fn func() error) error {
	return withFileLock(s.lockPath, func() error {
		if err := s.load(); err != nil {
			return err
		}
		return fn()
	})
}

// save writes profiles to disk. Callers must hold the write lock.
func (s *Store) save() error {
	if s.backupPending {
		if err := s.backupFile(); err != nil {
			return err
		}
		s.backupPending = false
		s.backupDone = true
	}

	profiles := make([]*Profile, 0, len(s.profiles)+len(s.unreadable))
	for _, p := range s.profiles {
		encrypted, err := s.encryptor.EncryptProfile(p)
		if err != nil {
			return fmt.Errorf("failed to encrypt profile %s: %w", p.Name, err)
		}
		profiles = append(profiles, encrypted)
	}
	for _, p := range s.unreadable {
		profiles = append(profiles, p)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })

	data, err := json.MarshalIndent(storeData{Version: 1, Profiles: profiles}, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal profiles: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return fmt.Errorf("failed to create profiles directory: %w", err)
	}
	if err := writeFileAtomic(s.path, data); err != nil {
		return fmt.Errorf("failed to write profiles file: %w", err)
	}
	return nil
}

// backupFile copies the current profiles file to
// profiles.json.<unix-timestamp>.bak.
func (s *Store) backupFile() error {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read profiles file for backup: %w", err)
	}
	backup := s.path + "." + strconv.FormatInt(time.Now().Unix(), 10) + ".bak"
	if err := writeFileAtomic(backup, data); err != nil {
		return fmt.Errorf("failed to back up profiles file: %w", err)
	}
	log.Printf("Backed up %s to %s before rewriting it", s.path, backup)
	return nil
}

// writeFileAtomic writes data to a 0600 temp file in path's directory, syncs
// it and renames it over path.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename

	if err := tmp.Chmod(0600); err != nil {
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
	return os.Rename(tmpPath, path)
}

// lockedView returns a copy of an undecryptable profile with its secret
// fields blanked and Locked set.
func lockedView(raw *Profile) *Profile {
	p := raw.clone()
	p.Token, p.Password, p.SecretKey, p.SessionToken, p.Kubeconfig = "", "", "", "", ""
	p.Locked = true
	return p
}

// LoadWarnings returns human-readable warnings produced while loading the
// store, such as profiles that could not be decrypted.
func (s *Store) LoadWarnings() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.warnings...)
}

// List returns copies of all profiles, including locked (undecryptable)
// profiles with their secrets blanked.
func (s *Store) List() []*Profile {
	s.mu.RLock()
	defer s.mu.RUnlock()

	profiles := make([]*Profile, 0, len(s.profiles)+len(s.unreadable))
	for _, p := range s.profiles {
		profiles = append(profiles, p.clone())
	}
	for _, p := range s.unreadable {
		profiles = append(profiles, lockedView(p))
	}
	return profiles
}

// Get returns a copy of the profile with the given ID. Locked profiles are
// returned with blank secrets and Locked set.
func (s *Store) Get(id string) (*Profile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if p, ok := s.profiles[id]; ok {
		return p.clone(), nil
	}
	if p, ok := s.unreadable[id]; ok {
		return lockedView(p), nil
	}
	return nil, fmt.Errorf("profile not found: %s", id)
}

// Create adds a new profile
func (s *Store) Create(req *ProfileCreateRequest) (*Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := uuid.New().String()
	profile := req.ToProfile(id)
	profile.ClusterAliases = maps.Clone(req.ClusterAliases)

	if err := profile.Validate(); err != nil {
		return nil, err
	}

	err := s.mutate(func() error {
		s.profiles[id] = profile
		if err := s.save(); err != nil {
			delete(s.profiles, id)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return profile.clone(), nil
}

// Update merges req onto an existing profile. Server-owned state
// (ExportedContexts, CreatedAt) is always kept, ClusterAliases are kept when
// the request omits them, and empty secret fields keep the stored secret
// unless the request switched to a different authentication method (see
// mergeSecrets). Updating a locked profile replaces it with a freshly
// encrypted copy and unlocks it.
func (s *Store) Update(id string, req *ProfileCreateRequest) (*Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var profile *Profile
	err := s.mutate(func() error {
		existing, readable := s.profiles[id]
		raw, locked := s.unreadable[id]
		if !readable && !locked {
			return fmt.Errorf("profile not found: %s", id)
		}
		base := existing
		if locked {
			base = lockedView(raw) // secrets cannot be carried over
		}

		profile = req.ToProfile(id)
		profile.CreatedAt = base.CreatedAt
		profile.ExportedContexts = maps.Clone(base.ExportedContexts)
		if req.ClusterAliases == nil {
			profile.ClusterAliases = maps.Clone(base.ClusterAliases)
		} else {
			profile.ClusterAliases = maps.Clone(req.ClusterAliases)
		}
		if err := mergeSecrets(profile, base, req); err != nil {
			return err
		}

		if err := profile.Validate(); err != nil {
			return err
		}

		s.profiles[id] = profile
		delete(s.unreadable, id)

		if err := s.save(); err != nil {
			if readable {
				s.profiles[id] = existing
			} else {
				delete(s.profiles, id)
				s.unreadable[id] = raw
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return profile.clone(), nil
}

// errReenterRancherSecret is returned when an update changes where a Rancher
// credential would be sent without supplying the credential again.
var errReenterRancherSecret = &ValidationError{
	Field:   "auth",
	Message: "re-enter the token or password when changing the Rancher URL, Skip TLS or CA certificate",
}

// mergeSecrets fills secret fields of p (built from req) from existing where
// the request left them empty. A secret is only carried over while the
// profile keeps the same type and the identity it belongs to, so switching
// auth mode (token <-> username/password, different access key) clears it.
//
// A Rancher token or password is also only carried over while the endpoint
// it is sent to is unchanged (same URL, CA certificate, and Skip TLS not
// newly enabled); otherwise a client could point a profile at another host
// and have the stored secret sent there. When the request changes the
// endpoint and leaves no credential, an error asks for it to be re-entered.
//
// The Rancher token is kept when the request sends no token and either no
// username or the stored username without a new password (the edit form
// always re-sends the username). A new password or a different username
// switches to username/password and clears the token.
func mergeSecrets(p, existing *Profile, req *ProfileCreateRequest) error {
	if req.Type != existing.Type {
		return nil
	}

	sameEndpoint := sameRancherEndpoint(req, existing)
	withheld := false

	switch {
	case req.Token != "":
	case req.Username == "" || (req.Username == existing.Username && req.Password == ""):
		if sameEndpoint {
			p.Token = existing.Token
		} else if existing.Token != "" {
			withheld = true
		}
	default:
		p.Token = "" // switched to username/password
	}

	if req.Password == "" && req.Username != "" && req.Username == existing.Username {
		if sameEndpoint {
			p.Password = existing.Password
		} else if existing.Password != "" {
			withheld = true
		}
	}

	if withheld && p.Token == "" && p.Password == "" {
		return errReenterRancherSecret
	}

	secretKept := false
	if req.SecretKey == "" && req.AccessKey != "" && req.AccessKey == existing.AccessKey {
		p.SecretKey = existing.SecretKey
		secretKept = true
	}

	if req.SessionToken == "" && secretKept {
		p.SessionToken = existing.SessionToken
	}

	if req.Kubeconfig == "" {
		p.Kubeconfig = existing.Kubeconfig
	}
	return nil
}

// sameRancherEndpoint reports whether req keeps the Rancher endpoint of
// existing: the same URL (scheme and host compared case-insensitively,
// trailing slashes ignored), the same CA certificate, and Skip TLS not newly
// enabled.
func sameRancherEndpoint(req *ProfileCreateRequest, existing *Profile) bool {
	if normalizeRancherURL(req.RancherURL) != normalizeRancherURL(existing.RancherURL) {
		return false
	}
	if strings.TrimSpace(req.CACert) != strings.TrimSpace(existing.CACert) {
		return false
	}
	return !req.SkipTLS || existing.SkipTLS
}

// normalizeRancherURL lowercases the scheme and host of raw and strips
// trailing slashes, so cosmetic differences do not count as a new endpoint.
func normalizeRancherURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.TrimRight(raw, "/")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return strings.TrimRight(u.String(), "/")
}

// Delete removes a profile (readable or locked).
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.mutate(func() error {
		if existing, ok := s.profiles[id]; ok {
			delete(s.profiles, id)
			if err := s.save(); err != nil {
				s.profiles[id] = existing
				return err
			}
			return nil
		}

		if raw, ok := s.unreadable[id]; ok {
			delete(s.unreadable, id)
			if err := s.save(); err != nil {
				s.unreadable[id] = raw
				return err
			}
			return nil
		}

		return fmt.Errorf("profile not found: %s", id)
	})
}

// Path returns the storage path
func (s *Store) Path() string {
	return s.path
}

// readableProfile returns the in-memory profile for id. Callers must hold
// the lock.
func (s *Store) readableProfile(id string) (*Profile, error) {
	if p, ok := s.profiles[id]; ok {
		return p, nil
	}
	if _, ok := s.unreadable[id]; ok {
		return nil, fmt.Errorf("profile %s is locked: its credentials could not be decrypted; re-enter them first", id)
	}
	return nil, fmt.Errorf("profile not found: %s", id)
}

// setMapEntry sets or (for an empty value) deletes key in a copy of m and
// returns the copy.
func setMapEntry(m map[string]string, key, value string) map[string]string {
	out := maps.Clone(m)
	if out == nil {
		out = make(map[string]string)
	}
	if value == "" {
		delete(out, key)
	} else {
		out[key] = value
	}
	return out
}

// SetClusterAlias sets or removes a cluster alias for a profile
func (s *Store) SetClusterAlias(profileID, clusterID, alias string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.mutate(func() error {
		profile, err := s.readableProfile(profileID)
		if err != nil {
			return err
		}

		oldAliases, oldUpdated := profile.ClusterAliases, profile.UpdatedAt
		profile.ClusterAliases = setMapEntry(profile.ClusterAliases, clusterID, alias)
		profile.UpdatedAt = time.Now()

		if err := s.save(); err != nil {
			profile.ClusterAliases, profile.UpdatedAt = oldAliases, oldUpdated
			return err
		}
		return nil
	})
}

// GetClusterAlias returns the alias for a cluster in a profile, or empty string if none
func (s *Store) GetClusterAlias(profileID, clusterID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	profile, exists := s.profiles[profileID]
	if !exists {
		profile, exists = s.unreadable[profileID]
	}
	if !exists {
		return ""
	}
	return profile.ClusterAliases[clusterID]
}

// SetExportedContext records that a cluster was merged to kubeconfig with the given context name
func (s *Store) SetExportedContext(profileID, clusterID, contextName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.mutate(func() error {
		profile, err := s.readableProfile(profileID)
		if err != nil {
			return err
		}

		oldExports, oldUpdated := profile.ExportedContexts, profile.UpdatedAt
		profile.ExportedContexts = setMapEntry(profile.ExportedContexts, clusterID, contextName)
		profile.UpdatedAt = time.Now()

		if err := s.save(); err != nil {
			profile.ExportedContexts, profile.UpdatedAt = oldExports, oldUpdated
			return err
		}
		return nil
	})
}

// GetExportedContext returns the exported context name for a cluster, or empty string if not exported
func (s *Store) GetExportedContext(profileID, clusterID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	profile, exists := s.profiles[profileID]
	if !exists {
		profile, exists = s.unreadable[profileID]
	}
	if !exists {
		return ""
	}
	return profile.ExportedContexts[clusterID]
}

// GetAllExportedContexts returns all exported context names across all profiles
func (s *Store) GetAllExportedContexts() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make(map[string]string)
	add := func(profile *Profile) {
		for clusterID, contextName := range profile.ExportedContexts {
			// Key by profileID:clusterID to avoid collisions
			result[profile.ID+":"+clusterID] = contextName
		}
	}
	for _, profile := range s.profiles {
		add(profile)
	}
	for _, profile := range s.unreadable {
		add(profile)
	}
	return result
}
