// Package web provides a web-based GUI for kubeconfig-wrangler
package web

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/kubeconfig-wrangler/pkg/config"
	kctx "github.com/kubeconfig-wrangler/pkg/context"
	"github.com/kubeconfig-wrangler/pkg/kubeconfig"
	"github.com/kubeconfig-wrangler/pkg/profile"
	"github.com/kubeconfig-wrangler/pkg/provider"
	"github.com/kubeconfig-wrangler/pkg/rancher"
)

// Server represents the web server
type Server struct {
	addr         string
	token        string
	mux          *http.ServeMux
	profileStore *profile.Store
	registry     *provider.Registry
	ctxSwitcher  *kctx.Switcher
}

// ClusterInfo holds cluster information for the API
type ClusterInfo struct {
	Name        string `json:"name"`
	ID          string `json:"id"`
	State       string `json:"state"`
	Provider    string `json:"provider"`
	Description string `json:"description"`
	Alias       string `json:"alias,omitempty"`
}

// GenerateRequest represents a kubeconfig generation request
type GenerateRequest struct {
	RancherURL            string   `json:"rancherUrl"`
	Token                 string   `json:"token"`
	Username              string   `json:"username"`
	Password              string   `json:"password"`
	ClusterPrefix         string   `json:"clusterPrefix"`
	InsecureSkipTLSVerify bool     `json:"insecureSkipTlsVerify"`
	SelectedClusters      []string `json:"selectedClusters"`
	AptakubeTags          []string `json:"aptakubeTags,omitempty"`
}

// APIResponse represents a generic API response
type APIResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// ClusterSelection represents a selected cluster with both ID and display name
type ClusterSelection struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Alias string `json:"alias,omitempty"`
}

// DisplayName returns the alias if set, otherwise the name
func (c ClusterSelection) DisplayName() string {
	if c.Alias != "" {
		return c.Alias
	}
	return c.Name
}

// NewServer creates a new web server
func NewServer(addr string, token string) *Server {
	store, err := profile.NewStore()
	if err != nil {
		log.Printf("Warning: failed to initialize profile store: %v", err)
	}

	s := &Server{
		addr:         addr,
		token:        token,
		mux:          http.NewServeMux(),
		profileStore: store,
		registry:     provider.NewRegistry(),
		ctxSwitcher:  kctx.NewSwitcher(),
	}
	s.setupRoutes()
	return s
}

// setupRoutes configures the HTTP routes
func (s *Server) setupRoutes() {
	s.mux.HandleFunc("/", s.handleIndex)

	// Legacy endpoints (for backwards compatibility)
	s.mux.HandleFunc("/api/clusters", s.handleListClusters)
	s.mux.HandleFunc("/api/generate", s.handleGenerate)

	// Profile management
	s.mux.HandleFunc("/api/profiles", s.handleProfiles)
	s.mux.HandleFunc("/api/profiles/", s.handleProfileByID)

	// Cluster operations with profiles
	s.mux.HandleFunc("/api/clusters/profile", s.handleListClustersForProfile)
	s.mux.HandleFunc("/api/clusters/all", s.handleListAllClusters)
	s.mux.HandleFunc("/api/generate/profile", s.handleGenerateForProfile)
	s.mux.HandleFunc("/api/generate/all", s.handleGenerateForAllSources)

	// AWS-specific endpoints
	s.mux.HandleFunc("/api/aws/profiles", s.handleAWSProfiles)
	s.mux.HandleFunc("/api/aws/regions", s.handleAWSRegions)

	// Context management
	s.mux.HandleFunc("/api/context", s.handleContext)
	s.mux.HandleFunc("/api/contexts", s.handleContexts)
	s.mux.HandleFunc("/api/context/merge", s.handleContextMerge)
	s.mux.HandleFunc("/api/context/delete", s.handleContextDelete)
	s.mux.HandleFunc("/api/context/rename", s.handleContextRename)

	// Cluster alias management
	s.mux.HandleFunc("/api/cluster/alias", s.handleClusterAlias)
}

// securityMiddleware validates requests for token auth and origin checks
func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip security checks for the index page (needs to serve HTML with token)
		if r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}

		// Only apply security to API endpoints
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		// Origin validation: only allow requests from same origin (localhost)
		origin := r.Header.Get("Origin")
		if origin != "" {
			// If Origin is set, it must be localhost
			if !strings.HasPrefix(origin, "http://127.0.0.1:") &&
				!strings.HasPrefix(origin, "http://localhost:") {
				log.Printf("Blocked request with invalid origin: %s", origin)
				http.Error(w, "Forbidden: invalid origin", http.StatusForbidden)
				return
			}
		}

		// Token validation (if token is configured)
		if s.token != "" {
			authToken := r.Header.Get("X-Auth-Token")
			if authToken != s.token {
				log.Printf("Blocked request with invalid or missing token")
				http.Error(w, "Unauthorized: invalid or missing token", http.StatusUnauthorized)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// Start starts the web server
func (s *Server) Start() error {
	log.Printf("Starting web server on %s", s.addr)
	if s.token != "" {
		log.Printf("Security: token authentication enabled")
	}
	log.Printf("Open http://%s in your browser", s.addr)

	// Wrap the mux with security middleware
	handler := s.securityMiddleware(s.mux)
	return http.ListenAndServe(s.addr, handler)
}

// templateData holds data passed to the HTML template
type templateData struct {
	AuthToken string
}

// handleIndex serves the main HTML page
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	tmpl, err := template.New("index").Parse(indexHTML)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	data := templateData{
		AuthToken: s.token,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("Error executing template: %v", err)
	}
}

// handleListClusters handles the cluster listing API endpoint
func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
		return
	}

	var req GenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	cfg := &config.Config{
		RancherURL:            req.RancherURL,
		Token:                 req.Token,
		Username:              req.Username,
		Password:              req.Password,
		InsecureSkipTLSVerify: req.InsecureSkipTLSVerify,
	}

	if err := cfg.Validate(); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Configuration error: %v", err),
		})
		return
	}

	client, err := rancher.NewClient(cfg)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to create Rancher client: %v", err),
		})
		return
	}

	clusters, err := client.ListClusters()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to list clusters: %v", err),
		})
		return
	}

	clusterInfos := make([]ClusterInfo, len(clusters))
	for i, c := range clusters {
		clusterInfos[i] = ClusterInfo{
			Name:        c.Name,
			ID:          c.ID,
			State:       c.State,
			Provider:    c.Provider,
			Description: c.Description,
		}
	}

	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    clusterInfos,
	})
}

// handleGenerate handles the kubeconfig generation API endpoint
func (s *Server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
		return
	}

	var req GenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	cfg := &config.Config{
		RancherURL:            req.RancherURL,
		Token:                 req.Token,
		Username:              req.Username,
		Password:              req.Password,
		ClusterPrefix:         req.ClusterPrefix,
		InsecureSkipTLSVerify: req.InsecureSkipTLSVerify,
	}

	if err := cfg.Validate(); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Configuration error: %v", err),
		})
		return
	}

	client, err := rancher.NewClient(cfg)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to create Rancher client: %v", err),
		})
		return
	}

	// Get list of clusters to determine which to include
	clusters, err := client.ListClusters()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to list clusters: %v", err),
		})
		return
	}

	// Build selected cluster set
	selectedSet := make(map[string]bool)
	for _, name := range req.SelectedClusters {
		selectedSet[name] = true
	}

	// Fetch kubeconfigs for selected clusters in parallel
	kubeconfigs := make(map[string]string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	errors := make([]string, 0)

	for _, cluster := range clusters {
		// Skip if not selected (when selection is provided)
		if len(selectedSet) > 0 && !selectedSet[cluster.Name] {
			continue
		}

		// Skip inactive clusters
		if cluster.State != "active" {
			continue
		}

		wg.Add(1)
		go func(c rancher.Cluster) {
			defer wg.Done()

			kubeconfig, err := client.GetClusterKubeconfig(&c)
			if err != nil {
				mu.Lock()
				errors = append(errors, fmt.Sprintf("%s: %v", c.Name, err))
				mu.Unlock()
				return
			}

			mu.Lock()
			kubeconfigs[c.Name] = kubeconfig
			mu.Unlock()
		}(cluster)
	}

	wg.Wait()

	if len(kubeconfigs) == 0 {
		errMsg := "No kubeconfigs retrieved"
		if len(errors) > 0 {
			errMsg = fmt.Sprintf("Failed to get kubeconfigs: %v", errors)
		}
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   errMsg,
		})
		return
	}

	// Generate merged kubeconfig
	generator := kubeconfig.NewGenerator(cfg.ClusterPrefix)
	// Add Aptakube tags if specified
	if len(req.AptakubeTags) > 0 {
		generator.SetAllTags(req.AptakubeTags)
	}
	kubeconfigData, err := generator.Generate(kubeconfigs)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to generate kubeconfig: %v", err),
		})
		return
	}

	// Return as downloadable file
	w.Header().Set("Content-Type", "application/x-yaml")
	w.Header().Set("Content-Disposition", "attachment; filename=kubeconfig.yaml")
	if _, err := w.Write(kubeconfigData); err != nil {
		log.Printf("Error writing kubeconfig response: %v", err)
	}
}

// writeJSON writes a JSON response
func (s *Server) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("Error encoding JSON response: %v", err)
	}
}

// sanitizeSourceName converts a source/profile name to a safe prefix format
// e.g., "My Rancher Server" -> "my-rancher-server"
func sanitizeSourceName(name string) string {
	// Convert to lowercase
	result := strings.ToLower(name)
	// Replace spaces and underscores with dashes
	result = strings.ReplaceAll(result, " ", "-")
	result = strings.ReplaceAll(result, "_", "-")
	// Remove any characters that aren't alphanumeric or dashes
	var cleaned strings.Builder
	for _, r := range result {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			cleaned.WriteRune(r)
		}
	}
	result = cleaned.String()
	// Remove consecutive dashes
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	// Trim leading/trailing dashes
	result = strings.Trim(result, "-")
	return result
}

// handleProfiles handles profile listing and creation
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listProfiles(w, r)
	case http.MethodPost:
		s.createProfile(w, r)
	default:
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
	}
}

func (s *Server) listProfiles(w http.ResponseWriter, r *http.Request) {
	if s.profileStore == nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   "Profile store not initialized",
		})
		return
	}

	profiles := s.profileStore.List()
	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    profiles,
	})
}

func (s *Server) createProfile(w http.ResponseWriter, r *http.Request) {
	if s.profileStore == nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   "Profile store not initialized",
		})
		return
	}

	var req profile.ProfileCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	p, err := s.profileStore.Create(&req)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to create profile: %v", err),
		})
		return
	}

	s.writeJSON(w, http.StatusCreated, APIResponse{
		Success: true,
		Data:    p,
	})
}

// handleProfileByID handles operations on a specific profile
func (s *Server) handleProfileByID(w http.ResponseWriter, r *http.Request) {
	if s.profileStore == nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   "Profile store not initialized",
		})
		return
	}

	// Extract profile ID from path
	id := strings.TrimPrefix(r.URL.Path, "/api/profiles/")
	if id == "" {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "Profile ID required",
		})
		return
	}

	switch r.Method {
	case http.MethodGet:
		p, err := s.profileStore.Get(id)
		if err != nil {
			s.writeJSON(w, http.StatusNotFound, APIResponse{
				Success: false,
				Error:   err.Error(),
			})
			return
		}
		s.writeJSON(w, http.StatusOK, APIResponse{
			Success: true,
			Data:    p,
		})

	case http.MethodPut:
		var req profile.ProfileCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("Invalid request: %v", err),
			})
			return
		}

		p, err := s.profileStore.Update(id, &req)
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("Failed to update profile: %v", err),
			})
			return
		}
		s.writeJSON(w, http.StatusOK, APIResponse{
			Success: true,
			Data:    p,
		})

	case http.MethodDelete:
		if err := s.profileStore.Delete(id); err != nil {
			s.writeJSON(w, http.StatusNotFound, APIResponse{
				Success: false,
				Error:   err.Error(),
			})
			return
		}
		// Also unregister the provider if it exists
		s.registry.Unregister(id)
		s.writeJSON(w, http.StatusOK, APIResponse{
			Success: true,
		})

	default:
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
	}
}

// markClustersInLocalKubeconfig checks which clusters exist in the local kubeconfig
// and sets the InLocalKubeconfig field accordingly
func (s *Server) markClustersInLocalKubeconfig(clusters []provider.ClusterInfo) {
	localNames, err := s.ctxSwitcher.GetLocalClusterNames()
	if err != nil {
		log.Printf("Warning: failed to get local cluster names: %v", err)
		return
	}

	for i := range clusters {
		// Check if the cluster name or alias exactly matches any local cluster/context name
		if localNames[clusters[i].Name] {
			clusters[i].InLocalKubeconfig = true
			continue
		}
		if clusters[i].Alias != "" && localNames[clusters[i].Alias] {
			clusters[i].InLocalKubeconfig = true
			continue
		}

		// Check if this cluster was exported (merged) with a tracked context name
		if s.profileStore != nil && clusters[i].ProfileID != "" {
			exportedContextName := s.profileStore.GetExportedContext(clusters[i].ProfileID, clusters[i].ID)
			if exportedContextName != "" && localNames[exportedContextName] {
				clusters[i].InLocalKubeconfig = true
			}
		}
	}
}

// handleListClustersForProfile lists clusters for a specific profile
func (s *Server) handleListClustersForProfile(w http.ResponseWriter, r *http.Request) {
	log.Printf("handleListClustersForProfile: received request")
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
		return
	}

	var req struct {
		ProfileID string `json:"profileId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	log.Printf("handleListClustersForProfile: profileId=%s", req.ProfileID)

	if s.profileStore == nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   "Profile store not initialized",
		})
		return
	}

	p, err := s.profileStore.Get(req.ProfileID)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	log.Printf("handleListClustersForProfile: profile type=%s, name=%s", p.Type, p.Name)

	// Create provider based on profile type
	clusters, err := s.getClustersForProfile(p)
	if err != nil {
		log.Printf("handleListClustersForProfile: error=%v", err)
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to list clusters: %v", err),
		})
		return
	}

	// Add aliases from profile
	if p.ClusterAliases != nil {
		for i := range clusters {
			if alias, exists := p.ClusterAliases[clusters[i].ID]; exists {
				clusters[i].Alias = alias
			}
		}
	}

	// Mark clusters that exist in local kubeconfig
	s.markClustersInLocalKubeconfig(clusters)

	log.Printf("handleListClustersForProfile: found %d clusters", len(clusters))
	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    clusters,
	})
}

func (s *Server) getClustersForProfile(p *profile.Profile) ([]provider.ClusterInfo, error) {
	switch p.Type {
	case profile.ProfileTypeRancher:
		cfg := provider.RancherConfig{
			ProfileID:     p.ID,
			ProfileName:   p.Name,
			URL:           p.RancherURL,
			Token:         p.Token,
			Username:      p.Username,
			Password:      p.Password,
			SkipTLSVerify: p.SkipTLS,
			CACert:        p.CACert,
		}
		prov, err := provider.NewRancherProvider(cfg)
		if err != nil {
			return nil, err
		}
		defer prov.Close()
		return prov.ListClusters()

	case profile.ProfileTypeEKS:
		cfg := provider.EKSConfig{
			ProfileID:    p.ID,
			ProfileName:  p.Name,
			Region:       p.AWSRegion,
			AWSProfile:   p.AWSProfile,
			AccessKey:    p.AccessKey,
			SecretKey:    p.SecretKey,
			SessionToken: p.SessionToken,
		}
		prov, err := provider.NewEKSProvider(cfg)
		if err != nil {
			return nil, err
		}
		defer prov.Close()
		return prov.ListClusters()

	case profile.ProfileTypeStatic:
		cfg := provider.StaticConfig{
			ProfileID:   p.ID,
			ProfileName: p.Name,
			Kubeconfig:  p.Kubeconfig,
		}
		prov, err := provider.NewStaticProvider(cfg)
		if err != nil {
			return nil, err
		}
		defer prov.Close()
		return prov.ListClusters()

	default:
		return nil, fmt.Errorf("unknown profile type: %s", p.Type)
	}
}

// handleListAllClusters returns clusters from all profiles
func (s *Server) handleListAllClusters(w http.ResponseWriter, r *http.Request) {
	log.Printf("handleListAllClusters: received request")
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
		return
	}

	if s.profileStore == nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   "Profile store not initialized",
		})
		return
	}

	allProfiles := s.profileStore.List()

	var allClusters []provider.ClusterInfo
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errors []string

	for _, p := range allProfiles {
		wg.Add(1)
		go func(prof *profile.Profile) {
			defer wg.Done()
			clusters, err := s.getClustersForProfile(prof)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				log.Printf("Error getting clusters for profile %s: %v", prof.Name, err)
				errors = append(errors, fmt.Sprintf("%s: %v", prof.Name, err))
				return
			}
			allClusters = append(allClusters, clusters...)
		}(p)
	}

	wg.Wait()

	// Mark clusters that exist in local kubeconfig
	s.markClustersInLocalKubeconfig(allClusters)

	log.Printf("handleListAllClusters: found %d clusters from %d profiles", len(allClusters), len(allProfiles))

	// Return clusters even if some profiles failed
	response := APIResponse{
		Success: true,
		Data:    allClusters,
	}
	if len(errors) > 0 {
		response.Error = fmt.Sprintf("Some sources failed: %s", strings.Join(errors, "; "))
	}

	s.writeJSON(w, http.StatusOK, response)
}

// handleGenerateForAllSources generates kubeconfig for selected clusters from any source
func (s *Server) handleGenerateForAllSources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
		return
	}

	var req struct {
		SelectedClusters []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Alias     string `json:"alias,omitempty"`
			ProfileID string `json:"profileId"`
		} `json:"selectedClusters"`
		ClusterPrefix   string   `json:"clusterPrefix"`
		AptakubeTags    []string `json:"aptakubeTags"`
		SeparateFiles   bool     `json:"separateFiles"`
		UseSourcePrefix bool     `json:"useSourcePrefix"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	if s.profileStore == nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   "Profile store not initialized",
		})
		return
	}

	// Group clusters by profile
	clustersByProfile := make(map[string][]ClusterSelection)
	for _, c := range req.SelectedClusters {
		clustersByProfile[c.ProfileID] = append(clustersByProfile[c.ProfileID], ClusterSelection{
			ID:    c.ID,
			Name:  c.Name,
			Alias: c.Alias,
		})
	}

	// Get kubeconfigs for each profile
	kubeconfigs := make(map[string]string)
	for profileID, clusters := range clustersByProfile {
		p, err := s.profileStore.Get(profileID)
		if err != nil {
			log.Printf("Profile not found: %s", profileID)
			continue
		}

		profileKubeconfigs, err := s.getKubeconfigsForProfile(p, clusters)
		if err != nil {
			log.Printf("Error getting kubeconfigs for profile %s: %v", p.Name, err)
			continue
		}

		// Apply source name prefix if requested
		sourcePrefix := ""
		if req.UseSourcePrefix {
			sourcePrefix = sanitizeSourceName(p.Name) + "-"
		}

		for name, kc := range profileKubeconfigs {
			prefixedName := sourcePrefix + name
			kubeconfigs[prefixedName] = kc
		}
	}

	if len(kubeconfigs) == 0 {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "No kubeconfigs retrieved for selected clusters",
		})
		return
	}

	generator := kubeconfig.NewGenerator(req.ClusterPrefix)
	if len(req.AptakubeTags) > 0 {
		generator.SetAllTags(req.AptakubeTags)
	}

	// Generate separate files if requested
	if req.SeparateFiles {
		zipBuffer := new(bytes.Buffer)
		zipWriter := zip.NewWriter(zipBuffer)

		for clusterName, kubeconfigStr := range kubeconfigs {
			singleConfig := map[string]string{clusterName: kubeconfigStr}
			kubeconfigData, err := generator.Generate(singleConfig)
			if err != nil {
				log.Printf("Error generating kubeconfig for %s: %v", clusterName, err)
				continue
			}

			safeFilename := strings.ReplaceAll(clusterName, "/", "-")
			safeFilename = strings.ReplaceAll(safeFilename, "\\", "-")
			filename := fmt.Sprintf("%s.yaml", safeFilename)

			fileWriter, err := zipWriter.Create(filename)
			if err != nil {
				log.Printf("Error creating zip entry for %s: %v", clusterName, err)
				continue
			}
			if _, err := fileWriter.Write(kubeconfigData); err != nil {
				log.Printf("Error writing zip entry for %s: %v", clusterName, err)
			}
		}

		if err := zipWriter.Close(); err != nil {
			s.writeJSON(w, http.StatusInternalServerError, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("Failed to create zip file: %v", err),
			})
			return
		}

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", "attachment; filename=kubeconfigs.zip")
		if _, err := w.Write(zipBuffer.Bytes()); err != nil {
			log.Printf("Error writing zip response: %v", err)
		}
		return
	}

	// Generate merged kubeconfig
	kubeconfigData, err := generator.Generate(kubeconfigs)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to generate kubeconfig: %v", err),
		})
		return
	}

	w.Header().Set("Content-Type", "application/x-yaml")
	w.Header().Set("Content-Disposition", "attachment; filename=kubeconfig.yaml")
	if _, err := w.Write(kubeconfigData); err != nil {
		log.Printf("Error writing kubeconfig response: %v", err)
	}
}

// handleAWSProfiles returns available AWS CLI profiles
func (s *Server) handleAWSProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
		return
	}

	profiles, err := provider.ListAWSProfiles()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to list AWS profiles: %v", err),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    profiles,
	})
}

// handleAWSRegions returns available AWS regions
func (s *Server) handleAWSRegions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
		return
	}

	regions := provider.ListAWSRegions()
	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    regions,
	})
}

// handleContext handles getting/setting the current context
func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ctx, err := s.ctxSwitcher.GetCurrentContext()
		if err != nil {
			s.writeJSON(w, http.StatusInternalServerError, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("Failed to get current context: %v", err),
			})
			return
		}
		s.writeJSON(w, http.StatusOK, APIResponse{
			Success: true,
			Data: map[string]string{
				"currentContext": ctx,
				"kubeconfigPath": s.ctxSwitcher.Path(),
			},
		})

	case http.MethodPut:
		var req struct {
			Context string `json:"context"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("Invalid request: %v", err),
			})
			return
		}

		if err := s.ctxSwitcher.SetCurrentContext(req.Context); err != nil {
			s.writeJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("Failed to set context: %v", err),
			})
			return
		}
		s.writeJSON(w, http.StatusOK, APIResponse{
			Success: true,
		})

	default:
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
	}
}

// handleContexts returns all available contexts
func (s *Server) handleContexts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
		return
	}

	contexts, err := s.ctxSwitcher.ListContexts()
	if err != nil {
		// If kubeconfig doesn't exist, return empty list
		if !s.ctxSwitcher.Exists() {
			s.writeJSON(w, http.StatusOK, APIResponse{
				Success: true,
				Data:    []kctx.ContextInfo{},
			})
			return
		}
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to list contexts: %v", err),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    contexts,
	})
}

// ExportedClusterInfo tracks cluster info for export tracking
type ExportedClusterInfo struct {
	ProfileID   string `json:"profileId"`
	ClusterID   string `json:"clusterId"`
	ContextName string `json:"contextName"`
}

// handleContextMerge merges a kubeconfig into the user's kubeconfig
func (s *Server) handleContextMerge(w http.ResponseWriter, r *http.Request) {
	log.Printf("handleContextMerge: received request")
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
		return
	}

	var req struct {
		Kubeconfig string                `json:"kubeconfig"`
		Clusters   []ExportedClusterInfo `json:"clusters,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("handleContextMerge: failed to decode request: %v", err)
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	if err := s.ctxSwitcher.MergeKubeconfig(req.Kubeconfig); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to merge kubeconfig: %v", err),
		})
		return
	}

	// Track exported clusters if profile store is available
	if s.profileStore != nil && len(req.Clusters) > 0 {
		for _, cluster := range req.Clusters {
			if cluster.ProfileID != "" && cluster.ClusterID != "" && cluster.ContextName != "" {
				if err := s.profileStore.SetExportedContext(cluster.ProfileID, cluster.ClusterID, cluster.ContextName); err != nil {
					log.Printf("handleContextMerge: failed to track export for cluster %s: %v", cluster.ClusterID, err)
				} else {
					log.Printf("handleContextMerge: tracked export %s -> %s", cluster.ClusterID, cluster.ContextName)
				}
			}
		}
	}

	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
	})
}

// handleContextDelete deletes a context from the kubeconfig
func (s *Server) handleContextDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
		return
	}

	var req struct {
		Name           string `json:"name"`
		CleanupOrphans bool   `json:"cleanupOrphans"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	if req.Name == "" {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "Context name is required",
		})
		return
	}

	if err := s.ctxSwitcher.DeleteContextWithCleanup(req.Name, req.CleanupOrphans); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to delete context: %v", err),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
	})
}

// handleContextRename renames a context in the kubeconfig
func (s *Server) handleContextRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
		return
	}

	var req struct {
		OldName string `json:"oldName"`
		NewName string `json:"newName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	if req.OldName == "" || req.NewName == "" {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "Both old and new context names are required",
		})
		return
	}

	if err := s.ctxSwitcher.RenameContext(req.OldName, req.NewName); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to rename context: %v", err),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
	})
}

// handleClusterAlias sets or removes a cluster alias for a profile
func (s *Server) handleClusterAlias(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
		return
	}

	if s.profileStore == nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   "Profile store not available",
		})
		return
	}

	var req struct {
		ProfileID string `json:"profileId"`
		ClusterID string `json:"clusterId"`
		Alias     string `json:"alias"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	if req.ProfileID == "" || req.ClusterID == "" {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "Profile ID and cluster ID are required",
		})
		return
	}

	if err := s.profileStore.SetClusterAlias(req.ProfileID, req.ClusterID, req.Alias); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to set cluster alias: %v", err),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
	})
}

// handleGenerateForProfile generates a kubeconfig for selected clusters from a profile
func (s *Server) handleGenerateForProfile(w http.ResponseWriter, r *http.Request) {
	log.Printf("handleGenerateForProfile: received request")
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
		return
	}

	var req struct {
		ProfileID        string             `json:"profileId"`
		SelectedClusters []ClusterSelection `json:"selectedClusters"`
		ClusterPrefix    string             `json:"clusterPrefix"`
		AptakubeTags     []string           `json:"aptakubeTags"`
		SeparateFiles    bool               `json:"separateFiles"`
		UseSourcePrefix  bool               `json:"useSourcePrefix"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("handleGenerateForProfile: failed to decode request: %v", err)
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}
	log.Printf("handleGenerateForProfile: profileId=%s, selectedClusters=%d", req.ProfileID, len(req.SelectedClusters))
	for i, c := range req.SelectedClusters {
		log.Printf("handleGenerateForProfile: cluster[%d] id=%s, name=%s", i, c.ID, c.Name)
	}

	if s.profileStore == nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   "Profile store not initialized",
		})
		return
	}

	p, err := s.profileStore.Get(req.ProfileID)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	// Get kubeconfigs for selected clusters
	rawKubeconfigs, err := s.getKubeconfigsForProfile(p, req.SelectedClusters)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to get kubeconfigs: %v", err),
		})
		return
	}

	// Apply source name prefix if requested
	kubeconfigs := make(map[string]string)
	sourcePrefix := ""
	if req.UseSourcePrefix {
		sourcePrefix = sanitizeSourceName(p.Name) + "-"
	}
	for name, kc := range rawKubeconfigs {
		prefixedName := sourcePrefix + name
		kubeconfigs[prefixedName] = kc
	}

	if len(kubeconfigs) == 0 {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "No kubeconfigs retrieved for selected clusters",
		})
		return
	}

	generator := kubeconfig.NewGenerator(req.ClusterPrefix)
	if len(req.AptakubeTags) > 0 {
		generator.SetAllTags(req.AptakubeTags)
	}

	// Generate separate files if requested
	if req.SeparateFiles {
		zipBuffer := new(bytes.Buffer)
		zipWriter := zip.NewWriter(zipBuffer)

		for clusterName, kubeconfigStr := range kubeconfigs {
			// Generate individual kubeconfig with prefix and tags
			singleConfig := map[string]string{clusterName: kubeconfigStr}
			kubeconfigData, err := generator.Generate(singleConfig)
			if err != nil {
				log.Printf("Error generating kubeconfig for %s: %v", clusterName, err)
				continue
			}

			// Create a safe filename
			safeFilename := strings.ReplaceAll(clusterName, "/", "-")
			safeFilename = strings.ReplaceAll(safeFilename, "\\", "-")
			filename := fmt.Sprintf("%s.yaml", safeFilename)

			fileWriter, err := zipWriter.Create(filename)
			if err != nil {
				log.Printf("Error creating zip entry for %s: %v", clusterName, err)
				continue
			}
			if _, err := fileWriter.Write(kubeconfigData); err != nil {
				log.Printf("Error writing zip entry for %s: %v", clusterName, err)
			}
		}

		if err := zipWriter.Close(); err != nil {
			s.writeJSON(w, http.StatusInternalServerError, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("Failed to create zip file: %v", err),
			})
			return
		}

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", "attachment; filename=kubeconfigs.zip")
		if _, err := w.Write(zipBuffer.Bytes()); err != nil {
			log.Printf("Error writing zip response: %v", err)
		}
		return
	}

	// Generate merged kubeconfig
	kubeconfigData, err := generator.Generate(kubeconfigs)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to generate kubeconfig: %v", err),
		})
		return
	}

	// Return as downloadable file
	w.Header().Set("Content-Type", "application/x-yaml")
	w.Header().Set("Content-Disposition", "attachment; filename=kubeconfig.yaml")
	if _, err := w.Write(kubeconfigData); err != nil {
		log.Printf("Error writing kubeconfig response: %v", err)
	}
}

// getKubeconfigsForProfile retrieves kubeconfigs for selected clusters from a profile
// Uses cluster.ID for API calls and cluster.Name as the key in the returned map for labeling
func (s *Server) getKubeconfigsForProfile(p *profile.Profile, selectedClusters []ClusterSelection) (map[string]string, error) {
	kubeconfigs := make(map[string]string)

	switch p.Type {
	case profile.ProfileTypeRancher:
		cfg := provider.RancherConfig{
			ProfileID:     p.ID,
			ProfileName:   p.Name,
			URL:           p.RancherURL,
			Token:         p.Token,
			Username:      p.Username,
			Password:      p.Password,
			SkipTLSVerify: p.SkipTLS,
			CACert:        p.CACert,
		}
		prov, err := provider.NewRancherProvider(cfg)
		if err != nil {
			return nil, err
		}
		defer prov.Close()

		for _, cluster := range selectedClusters {
			// Use ID for API call, DisplayName (alias or name) for labeling
			kc, err := prov.GetKubeconfig(cluster.ID)
			if err != nil {
				log.Printf("Warning: failed to get kubeconfig for cluster %s (id: %s): %v", cluster.Name, cluster.ID, err)
				continue
			}
			kubeconfigs[cluster.DisplayName()] = kc
		}

	case profile.ProfileTypeEKS:
		cfg := provider.EKSConfig{
			ProfileID:    p.ID,
			ProfileName:  p.Name,
			Region:       p.AWSRegion,
			AWSProfile:   p.AWSProfile,
			AccessKey:    p.AccessKey,
			SecretKey:    p.SecretKey,
			SessionToken: p.SessionToken,
		}
		prov, err := provider.NewEKSProvider(cfg)
		if err != nil {
			return nil, err
		}
		defer prov.Close()

		for _, cluster := range selectedClusters {
			// For EKS, use DisplayName (alias or name) for labeling
			kc, err := prov.GetKubeconfig(cluster.ID)
			if err != nil {
				log.Printf("Warning: failed to get kubeconfig for cluster %s: %v", cluster.Name, err)
				continue
			}
			kubeconfigs[cluster.DisplayName()] = kc
		}

	case profile.ProfileTypeStatic:
		cfg := provider.StaticConfig{
			ProfileID:   p.ID,
			ProfileName: p.Name,
			Kubeconfig:  p.Kubeconfig,
		}
		prov, err := provider.NewStaticProvider(cfg)
		if err != nil {
			return nil, err
		}
		defer prov.Close()

		for _, cluster := range selectedClusters {
			// Use DisplayName (alias or name) for labeling
			kc, err := prov.GetKubeconfig(cluster.ID)
			if err != nil {
				log.Printf("Warning: failed to get kubeconfig for cluster %s: %v", cluster.Name, err)
				continue
			}
			kubeconfigs[cluster.DisplayName()] = kc
		}

	default:
		return nil, fmt.Errorf("unknown profile type: %s", p.Type)
	}

	return kubeconfigs, nil
}

// indexHTML is the embedded HTML template for the web interface
const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <meta name="auth-token" content="{{.AuthToken}}">
    <title>Kubeconfig Wrangler</title>
    <style>
        :root {
            --bg-primary: #1e1e1e;
            --bg-secondary: #252526;
            --bg-tertiary: #2d2d30;
            --bg-hover: #3c3c3c;
            --bg-active: #094771;
            --text-primary: #cccccc;
            --text-secondary: #969696;
            --text-bright: #ffffff;
            --accent: #0078d4;
            --accent-hover: #1c8ae6;
            --success: #4ec9b0;
            --warning: #dcdcaa;
            --error: #f14c4c;
            --border: #3c3c3c;
            --input-bg: #3c3c3c;
            --sidebar-width: 320px;
        }

        @media (prefers-color-scheme: light) {
            :root {
                --bg-primary: #ffffff;
                --bg-secondary: #f3f3f3;
                --bg-tertiary: #e8e8e8;
                --bg-hover: #e0e0e0;
                --bg-active: #cce4f7;
                --text-primary: #333333;
                --text-secondary: #666666;
                --text-bright: #000000;
                --accent: #0078d4;
                --accent-hover: #106ebe;
                --success: #107c10;
                --warning: #ca8a00;
                --error: #d32f2f;
                --border: #d4d4d4;
                --input-bg: #ffffff;
            }
        }

        * {
            box-sizing: border-box;
            margin: 0;
            padding: 0;
        }

        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
            background: var(--bg-primary);
            color: var(--text-primary);
            font-size: 13px;
            line-height: 1.4;
            height: 100vh;
            overflow: hidden;
            display: flex;
            flex-direction: column;
        }

        /* Toolbar */
        .toolbar {
            height: 38px;
            background: var(--bg-secondary);
            border-bottom: 1px solid var(--border);
            display: flex;
            align-items: center;
            padding: 0 12px;
            gap: 8px;
            -webkit-app-region: drag;
            flex-shrink: 0;
        }

        .toolbar-title {
            font-weight: 600;
            font-size: 13px;
            color: var(--text-bright);
            margin-right: auto;
        }

        .toolbar-btn {
            -webkit-app-region: no-drag;
            background: var(--accent);
            color: white;
            border: none;
            padding: 5px 12px;
            border-radius: 3px;
            font-size: 12px;
            font-weight: 500;
            cursor: pointer;
            display: flex;
            align-items: center;
            gap: 6px;
        }

        .toolbar-btn:hover {
            background: var(--accent-hover);
        }

        .toolbar-btn:disabled {
            background: var(--bg-tertiary);
            color: var(--text-secondary);
            cursor: not-allowed;
        }

        .toolbar-btn-secondary {
            background: var(--bg-tertiary);
            color: var(--text-primary);
        }

        .toolbar-btn-secondary:hover {
            background: var(--bg-hover);
        }

        /* Main Layout */
        .main-container {
            display: flex;
            flex: 1;
            overflow: hidden;
        }

        /* Sidebar */
        .sidebar {
            width: var(--sidebar-width);
            background: var(--bg-secondary);
            border-right: 1px solid var(--border);
            display: flex;
            flex-direction: column;
            flex-shrink: 0;
        }

        .sidebar-header {
            padding: 12px;
            font-weight: 600;
            font-size: 11px;
            text-transform: uppercase;
            letter-spacing: 0.5px;
            color: var(--text-secondary);
            border-bottom: 1px solid var(--border);
            display: flex;
            justify-content: space-between;
            align-items: center;
        }

        .add-btn {
            background: var(--accent);
            color: white;
            border: none;
            width: 20px;
            height: 20px;
            border-radius: 3px;
            font-size: 14px;
            font-weight: bold;
            cursor: pointer;
            display: flex;
            align-items: center;
            justify-content: center;
        }

        .add-btn:hover {
            background: var(--accent-hover);
        }

        .sidebar-content {
            flex: 1;
            overflow-y: auto;
            padding: 12px;
        }

        .sidebar-divider {
            height: 1px;
            background: var(--border);
            margin: 16px 0;
        }

        .profiles-section {
            margin-bottom: 8px;
        }

        .profile-list {
            display: flex;
            flex-direction: column;
            gap: 4px;
        }

        .empty-profiles {
            color: var(--text-secondary);
            font-size: 11px;
            font-style: italic;
            padding: 8px 0;
        }

        .profile-item {
            display: flex;
            align-items: center;
            gap: 8px;
            padding: 8px;
            background: var(--bg-tertiary);
            border-radius: 4px;
            cursor: pointer;
        }

        .profile-item:hover {
            background: var(--bg-hover);
        }

        .profile-item.active {
            background: var(--bg-active);
        }

        .profile-separator {
            height: 1px;
            background: var(--border);
            margin: 4px 8px;
        }

        .profile-icon {
            width: 20px;
            height: 20px;
            display: flex;
            align-items: center;
            justify-content: center;
            font-size: 12px;
        }

        .profile-info {
            flex: 1;
            min-width: 0;
        }

        .profile-name {
            font-size: 12px;
            font-weight: 500;
            color: var(--text-primary);
            white-space: nowrap;
            overflow: hidden;
            text-overflow: ellipsis;
        }

        .profile-type {
            font-size: 10px;
            color: var(--text-secondary);
        }

        .profile-actions {
            display: flex;
            gap: 2px;
            opacity: 0;
            transition: opacity 0.1s;
        }

        .profile-item:hover .profile-actions {
            opacity: 1;
        }

        .profile-action-btn {
            background: none;
            border: none;
            color: var(--text-secondary);
            cursor: pointer;
            padding: 4px;
            font-size: 12px;
            border-radius: 3px;
        }

        .profile-action-btn:hover {
            background: var(--bg-hover);
            color: var(--text-primary);
        }

        .profile-action-btn.delete:hover {
            color: var(--error);
        }

        /* Modal */
        .modal-overlay {
            position: fixed;
            top: 0;
            left: 0;
            right: 0;
            bottom: 0;
            background: rgba(0, 0, 0, 0.5);
            display: none;
            align-items: center;
            justify-content: center;
            z-index: 1000;
        }

        .modal-overlay.show {
            display: flex;
        }

        .modal {
            background: var(--bg-secondary);
            border-radius: 8px;
            width: 400px;
            max-height: 80vh;
            overflow: hidden;
            box-shadow: 0 4px 20px rgba(0, 0, 0, 0.3);
        }

        .modal-header {
            padding: 16px;
            border-bottom: 1px solid var(--border);
            display: flex;
            justify-content: space-between;
            align-items: center;
        }

        .modal-title {
            font-size: 14px;
            font-weight: 600;
            color: var(--text-bright);
        }

        .modal-close {
            background: none;
            border: none;
            color: var(--text-secondary);
            cursor: pointer;
            font-size: 18px;
            padding: 4px;
        }

        .modal-close:hover {
            color: var(--text-primary);
        }

        .modal-body {
            padding: 16px;
            overflow-y: auto;
            max-height: 60vh;
        }

        .modal-footer {
            padding: 16px;
            border-top: 1px solid var(--border);
            display: flex;
            justify-content: flex-end;
            gap: 8px;
        }

        .type-tabs {
            display: flex;
            gap: 4px;
            margin-bottom: 16px;
        }

        .type-tab {
            flex: 1;
            padding: 8px;
            background: var(--bg-tertiary);
            border: none;
            border-radius: 4px;
            color: var(--text-secondary);
            font-size: 12px;
            cursor: pointer;
            text-align: center;
        }

        .type-tab:hover {
            background: var(--bg-hover);
        }

        .type-tab.active {
            background: var(--accent);
            color: white;
        }

        .type-panel {
            display: none;
        }

        .type-panel.active {
            display: block;
        }

        .form-group select,
        .form-group textarea {
            width: 100%;
            padding: 6px 8px;
            background: var(--input-bg);
            border: 1px solid var(--border);
            border-radius: 3px;
            color: var(--text-primary);
            font-size: 12px;
            font-family: inherit;
        }

        .form-group textarea {
            min-height: 100px;
            resize: vertical;
            font-family: monospace;
        }

        .form-group select:focus,
        .form-group textarea:focus {
            outline: none;
            border-color: var(--accent);
        }

        .form-section {
            margin-bottom: 16px;
        }

        .form-section-title {
            font-size: 11px;
            font-weight: 600;
            color: var(--text-secondary);
            text-transform: uppercase;
            letter-spacing: 0.5px;
            margin-bottom: 8px;
        }

        .form-group {
            margin-bottom: 12px;
        }

        .form-group label {
            display: block;
            font-size: 12px;
            color: var(--text-secondary);
            margin-bottom: 4px;
        }

        .form-group input[type="text"],
        .form-group input[type="password"],
        .form-group input[type="url"] {
            width: 100%;
            padding: 6px 8px;
            background: var(--input-bg);
            border: 1px solid var(--border);
            border-radius: 3px;
            color: var(--text-primary);
            font-size: 12px;
            font-family: inherit;
        }

        .form-group input:focus {
            outline: none;
            border-color: var(--accent);
        }

        .form-group input::placeholder {
            color: var(--text-secondary);
        }

        /* Segmented Control */
        .segmented-control {
            display: flex;
            background: var(--bg-tertiary);
            border-radius: 4px;
            padding: 2px;
            margin-bottom: 12px;
        }

        .segment-btn {
            flex: 1;
            padding: 5px 8px;
            border: none;
            background: transparent;
            color: var(--text-secondary);
            font-size: 11px;
            font-weight: 500;
            cursor: pointer;
            border-radius: 3px;
            transition: all 0.15s;
        }

        .segment-btn:hover {
            color: var(--text-primary);
        }

        .segment-btn.active {
            background: var(--accent);
            color: white;
        }

        .auth-panel {
            display: none;
        }

        .auth-panel.active {
            display: block;
        }

        /* Checkbox */
        .checkbox-row {
            display: flex;
            align-items: center;
            gap: 8px;
            padding: 4px 0;
        }

        .checkbox-row input[type="checkbox"] {
            width: 14px;
            height: 14px;
            accent-color: var(--accent);
        }

        .checkbox-row label {
            font-size: 12px;
            color: var(--text-primary);
            cursor: pointer;
            margin: 0;
        }

        /* Main Content */
        .content {
            flex: 1;
            display: flex;
            flex-direction: column;
            overflow: hidden;
        }

        .content-header {
            padding: 12px 16px;
            background: var(--bg-secondary);
            border-bottom: 1px solid var(--border);
            display: flex;
            align-items: center;
            justify-content: space-between;
        }

        .content-title {
            font-weight: 600;
            font-size: 12px;
            color: var(--text-bright);
        }

        .cluster-count {
            font-size: 11px;
            color: var(--text-secondary);
            background: var(--bg-tertiary);
            padding: 2px 8px;
            border-radius: 10px;
        }

        /* Table */
        .table-container {
            flex: 1;
            overflow: auto;
        }

        .cluster-table {
            width: 100%;
            border-collapse: collapse;
        }

        .cluster-table th {
            position: sticky;
            top: 0;
            background: var(--bg-tertiary);
            text-align: left;
            padding: 8px 12px;
            font-size: 11px;
            font-weight: 600;
            color: var(--text-secondary);
            text-transform: uppercase;
            letter-spacing: 0.5px;
            border-bottom: 1px solid var(--border);
        }

        .cluster-table th:first-child {
            width: 40px;
            text-align: center;
        }

        .cluster-table td {
            padding: 10px 12px;
            border-bottom: 1px solid var(--border);
            vertical-align: middle;
        }

        .cluster-table tr:hover {
            background: var(--bg-hover);
        }

        .cluster-table tr.selected {
            background: var(--bg-active);
        }

        .cluster-table td:first-child {
            text-align: center;
        }

        .cluster-name {
            font-weight: 500;
            color: var(--text-bright);
        }

        .cluster-name-cell {
            padding: 8px 12px;
        }

        .cluster-name-row {
            display: flex;
            align-items: center;
            gap: 4px;
        }

        .cluster-display-name {
            color: var(--text-primary);
            font-weight: 500;
        }

        .cluster-source-name {
            font-size: 11px;
            color: var(--text-secondary);
            margin-top: 2px;
            font-family: monospace;
        }

        .edit-alias-btn {
            background: transparent;
            border: none;
            cursor: pointer;
            padding: 2px 4px;
            font-size: 11px;
            opacity: 0.4;
            transition: opacity 0.2s;
        }

        .edit-alias-btn:hover {
            opacity: 1;
        }

        .cluster-id {
            font-size: 11px;
            color: var(--text-secondary);
            font-family: 'SF Mono', Monaco, 'Courier New', monospace;
        }

        .status-badge {
            display: inline-flex;
            align-items: center;
            gap: 4px;
            padding: 2px 8px;
            border-radius: 10px;
            font-size: 11px;
            font-weight: 500;
        }

        .status-badge::before {
            content: '';
            width: 6px;
            height: 6px;
            border-radius: 50%;
        }

        .status-active {
            background: rgba(78, 201, 176, 0.15);
            color: var(--success);
        }

        .status-active::before {
            background: var(--success);
        }

        .status-inactive {
            background: rgba(220, 220, 170, 0.15);
            color: var(--warning);
        }

        .status-inactive::before {
            background: var(--warning);
        }

        .status-error {
            background: rgba(241, 76, 76, 0.15);
            color: var(--error);
        }

        .status-error::before {
            background: var(--error);
        }

        /* Local kubeconfig indicator */
        .local-config-badge {
            display: inline-flex;
            align-items: center;
            gap: 3px;
            padding: 1px 6px;
            border-radius: 8px;
            font-size: 10px;
            font-weight: 500;
            background: rgba(78, 201, 176, 0.15);
            color: var(--success);
            margin-left: 8px;
        }

        .local-config-badge::before {
            content: '✓';
        }

        /* Empty State */
        .empty-state {
            display: flex;
            flex-direction: column;
            align-items: center;
            justify-content: center;
            height: 100%;
            color: var(--text-secondary);
            text-align: center;
            padding: 40px;
        }

        .empty-state-icon {
            font-size: 48px;
            margin-bottom: 16px;
            opacity: 0.5;
        }

        .empty-state-title {
            font-size: 16px;
            font-weight: 500;
            color: var(--text-primary);
            margin-bottom: 8px;
        }

        .empty-state-text {
            font-size: 13px;
            max-width: 300px;
        }

        /* Loading */
        .loading-overlay {
            display: none;
            position: absolute;
            top: 0;
            left: 0;
            right: 0;
            bottom: 0;
            background: rgba(0, 0, 0, 0.5);
            align-items: center;
            justify-content: center;
            z-index: 100;
        }

        .loading-overlay.visible {
            display: flex;
        }

        .loading-spinner {
            width: 32px;
            height: 32px;
            border: 3px solid var(--border);
            border-top-color: var(--accent);
            border-radius: 50%;
            animation: spin 0.8s linear infinite;
        }

        @keyframes spin {
            to { transform: rotate(360deg); }
        }

        /* Options Panel */
        .options-panel {
            background: var(--bg-secondary);
            border-top: 1px solid var(--border);
            margin-top: auto;
        }
        .options-header {
            display: flex;
            justify-content: space-between;
            align-items: center;
            padding: 8px 16px;
            cursor: pointer;
            font-size: 12px;
            font-weight: 500;
            color: var(--text-secondary);
        }
        .options-header:hover {
            background: var(--bg-tertiary);
        }
        .options-toggle {
            transition: transform 0.2s;
        }
        .options-panel.collapsed .options-toggle {
            transform: rotate(-90deg);
        }
        .options-panel.collapsed .options-content {
            display: none;
        }
        .options-content {
            padding: 12px 16px;
            border-top: 1px solid var(--border);
        }
        .options-row {
            display: flex;
            gap: 16px;
            margin-bottom: 12px;
        }
        .options-row:last-child {
            margin-bottom: 0;
        }
        .option-group {
            flex: 1;
        }
        .option-group label {
            display: block;
            font-size: 11px;
            font-weight: 500;
            color: var(--text-secondary);
            margin-bottom: 4px;
        }
        .option-group input[type="text"] {
            width: 100%;
            padding: 6px 8px;
            background: var(--bg-primary);
            border: 1px solid var(--border);
            border-radius: 4px;
            color: var(--text-primary);
            font-size: 12px;
        }
        .option-group input[type="text"]:focus {
            outline: none;
            border-color: var(--accent);
        }
        .option-hint {
            display: block;
            font-size: 10px;
            color: var(--text-muted);
            margin-top: 4px;
        }
        .prefix-preview {
            font-size: 11px;
            color: var(--text-secondary);
            margin-top: 4px;
            padding: 4px 8px;
            background: var(--bg-tertiary);
            border-radius: 3px;
            font-family: monospace;
            display: none;
        }
        .prefix-preview.visible {
            display: block;
        }
        .prefix-preview .prefix-highlight {
            color: var(--accent);
            font-weight: 500;
        }
        .option-checkbox label {
            display: flex;
            align-items: center;
            gap: 8px;
            cursor: pointer;
            font-size: 12px;
            color: var(--text-primary);
        }
        .option-checkbox input[type="checkbox"] {
            width: 14px;
            height: 14px;
            cursor: pointer;
        }

        /* Status Bar */
        .status-bar {
            height: 24px;
            background: var(--bg-tertiary);
            border-top: 1px solid var(--border);
            display: flex;
            align-items: center;
            padding: 0 12px;
            font-size: 11px;
            color: var(--text-secondary);
            flex-shrink: 0;
        }

        .status-bar-item {
            display: flex;
            align-items: center;
            gap: 6px;
        }

        .status-bar-item::after {
            content: '|';
            margin: 0 8px;
            opacity: 0.3;
        }

        .status-bar-item:last-child::after {
            display: none;
        }

        .status-dot {
            width: 8px;
            height: 8px;
            border-radius: 50%;
            background: var(--text-secondary);
        }

        .status-dot.connected {
            background: var(--success);
        }

        .status-dot.error {
            background: var(--error);
        }

        /* Toast Notification */
        .toast {
            position: fixed;
            bottom: 40px;
            left: 50%;
            transform: translateX(-50%) translateY(100px);
            background: var(--bg-tertiary);
            border: 1px solid var(--border);
            border-radius: 6px;
            padding: 12px 20px;
            font-size: 13px;
            box-shadow: 0 4px 12px rgba(0, 0, 0, 0.3);
            z-index: 1000;
            opacity: 0;
            transition: all 0.3s ease;
        }

        .toast.visible {
            transform: translateX(-50%) translateY(0);
            opacity: 1;
        }

        .toast.success {
            border-left: 3px solid var(--success);
        }

        .toast.error {
            border-left: 3px solid var(--error);
        }

        .hidden {
            display: none !important;
        }
    </style>
</head>
<body>
    <!-- Toolbar -->
    <div class="toolbar">
        <span class="toolbar-title">Kubeconfig Wrangler</span>
        <button class="toolbar-btn toolbar-btn-secondary" onclick="showManageKubeconfigModal()">
            ⚙ Manage Kubeconfig
        </button>
        <button class="toolbar-btn toolbar-btn-secondary" id="refreshBtn" onclick="fetchClusters()" disabled>
            ↻ Refresh
        </button>
        <button class="toolbar-btn" id="generateBtn" onclick="generateKubeconfig()" disabled>
            ⬇ Generate Kubeconfig
        </button>
    </div>

    <!-- Main Container -->
    <div class="main-container">
        <!-- Sidebar -->
        <div class="sidebar">
            <div class="sidebar-header">
                Connections
                <button class="add-btn" onclick="showAddConnectionModal()" title="Add Connection">+</button>
            </div>
            <div class="sidebar-content">
                <!-- Saved Profiles -->
                <div class="profiles-section" id="profilesSection">
                    <div class="profile-list" id="profileList">
                        <div class="empty-profiles">No saved connections. Click + to add one.</div>
                    </div>
                </div>
            </div>
        </div>

        <!-- Content -->
        <div class="content">
            <div class="content-header">
                <span class="content-title">Clusters</span>
                <span class="cluster-count" id="clusterCount">0 clusters</span>
            </div>

            <div class="table-container" id="tableContainer">
                <!-- Empty State -->
                <div class="empty-state" id="emptyState">
                    <div class="empty-state-icon">☁</div>
                    <div class="empty-state-title">No clusters loaded</div>
                    <div class="empty-state-text">Add a connection using the + button, then select it to load clusters.</div>
                </div>

                <!-- Cluster Table -->
                <table class="cluster-table hidden" id="clusterTable">
                    <thead>
                        <tr>
                            <th><input type="checkbox" id="selectAll" onchange="toggleSelectAll()"></th>
                            <th>Cluster</th>
                            <th>Source</th>
                            <th>Provider</th>
                            <th>Status</th>
                        </tr>
                    </thead>
                    <tbody id="clusterTableBody">
                    </tbody>
                </table>
            </div>

            <!-- Options Panel -->
            <div class="options-panel" id="optionsPanel">
                <div class="options-header" onclick="toggleOptionsPanel()">
                    <span>Generation Options</span>
                    <span class="options-toggle" id="optionsToggle">▼</span>
                </div>
                <div class="options-content" id="optionsContent">
                    <div class="options-row">
                        <div class="option-group">
                            <label>Cluster Name Prefix</label>
                            <input type="text" id="clusterPrefix" placeholder="e.g., prod-" oninput="updatePrefixPreview()">
                            <div class="prefix-preview" id="prefixPreview"></div>
                        </div>
                        <div class="option-group">
                            <label>Aptakube Tags</label>
                            <input type="text" id="aptakubeTags" placeholder="tag1, tag2, tag3">
                            <span class="option-hint">Comma-separated tags for Aptakube</span>
                        </div>
                    </div>
                    <div class="options-row">
                        <div class="option-group option-checkbox">
                            <label>
                                <input type="checkbox" id="useSourcePrefix">
                                Use source name as prefix
                            </label>
                            <span class="option-hint">Prefix cluster contexts with source name (e.g., my-rancher-cluster-name)</span>
                        </div>
                        <div class="option-group option-checkbox">
                            <label>
                                <input type="checkbox" id="separateFiles">
                                Generate separate file per source
                            </label>
                            <span class="option-hint">Download a zip with one kubeconfig per connection</span>
                        </div>
                    </div>
                    <div class="options-row">
                        <div class="option-group option-checkbox">
                            <label>
                                <input type="checkbox" id="mergeToKubeconfig">
                                Merge to ~/.kube/config
                            </label>
                            <span class="option-hint">Merge generated contexts directly into your default kubeconfig file</span>
                        </div>
                    </div>
                </div>
            </div>

            <!-- Loading Overlay -->
            <div class="loading-overlay" id="loadingOverlay">
                <div class="loading-spinner"></div>
            </div>
        </div>
    </div>

    <!-- Status Bar -->
    <div class="status-bar">
        <div class="status-bar-item">
            <span class="status-dot" id="connectionStatus"></span>
            <span id="connectionText">Not connected</span>
        </div>
        <div class="status-bar-item" id="selectionStatus">
            0 selected
        </div>
    </div>

    <!-- Add/Edit Connection Modal -->
    <div class="modal-overlay" id="addConnectionModal">
        <div class="modal">
            <div class="modal-header">
                <span class="modal-title" id="modalTitle">Add Connection</span>
                <button class="modal-close" onclick="hideAddConnectionModal()">&times;</button>
            </div>
            <div class="modal-body">
                <div class="form-group">
                    <label>Connection Name</label>
                    <input type="text" id="newProfileName" placeholder="My Cluster">
                </div>

                <div class="type-tabs">
                    <button class="type-tab active" onclick="switchProfileType('rancher', this)">Rancher</button>
                    <button class="type-tab" onclick="switchProfileType('eks', this)">AWS EKS</button>
                    <button class="type-tab" onclick="switchProfileType('static', this)">Manual</button>
                </div>

                <!-- Rancher Panel -->
                <div class="type-panel active" id="panelRancher">
                    <div class="form-group">
                        <label>Rancher URL</label>
                        <input type="url" id="newRancherUrl" placeholder="https://rancher.example.com">
                    </div>
                    <div class="form-group">
                        <label>API Token</label>
                        <input type="password" id="newRancherToken" placeholder="token-xxxxx:yyyyyyy">
                    </div>
                    <div class="form-section-title">Or use username/password</div>
                    <div class="form-group">
                        <label>Username</label>
                        <input type="text" id="newRancherUsername" placeholder="admin">
                    </div>
                    <div class="form-group">
                        <label>Password</label>
                        <input type="password" id="newRancherPassword" placeholder="Password">
                    </div>
                    <div class="checkbox-row">
                        <input type="checkbox" id="newRancherSkipTls">
                        <label for="newRancherSkipTls">Skip TLS verification</label>
                    </div>
                </div>

                <!-- EKS Panel -->
                <div class="type-panel" id="panelEks">
                    <div class="form-group">
                        <label>AWS Region</label>
                        <select id="newEksRegion">
                            <option value="">Select region...</option>
                        </select>
                    </div>
                    <div class="form-group">
                        <label>AWS Profile (from ~/.aws/credentials)</label>
                        <select id="newEksProfile">
                            <option value="">Select profile or enter credentials below...</option>
                        </select>
                    </div>
                    <div class="form-section-title">Or use direct credentials</div>
                    <div class="form-group">
                        <label>Access Key ID</label>
                        <input type="text" id="newEksAccessKey" placeholder="AKIA...">
                    </div>
                    <div class="form-group">
                        <label>Secret Access Key</label>
                        <input type="password" id="newEksSecretKey" placeholder="Secret key">
                    </div>
                </div>

                <!-- Static Panel -->
                <div class="type-panel" id="panelStatic">
                    <div class="form-group">
                        <label>Kubeconfig Content</label>
                        <div style="display: flex; gap: 8px; margin-bottom: 8px;">
                            <button class="toolbar-btn toolbar-btn-secondary" onclick="loadKubeconfigFromFile()" style="flex: 1; justify-content: center;">
                                📂 Load from File
                            </button>
                        </div>
                        <textarea id="newStaticKubeconfig" placeholder="Paste your kubeconfig YAML here or click 'Load from File' above..."></textarea>
                    </div>
                </div>
            </div>
            <div class="modal-footer">
                <button class="toolbar-btn toolbar-btn-secondary" onclick="hideAddConnectionModal()">Cancel</button>
                <button class="toolbar-btn" onclick="saveNewProfile()">Save Connection</button>
            </div>
        </div>
    </div>

    <!-- Manage Kubeconfig Modal -->
    <div class="modal-overlay" id="manageKubeconfigModal">
        <div class="modal" style="max-width: 700px;">
            <div class="modal-header">
                <span class="modal-title">Manage Kubeconfig</span>
                <button class="modal-close" onclick="hideManageKubeconfigModal()">&times;</button>
            </div>
            <div class="modal-body">
                <div class="form-group">
                    <label>Kubeconfig Path</label>
                    <input type="text" id="kubeconfigPath" readonly style="background: var(--bg-tertiary); cursor: default;">
                </div>
                <div class="form-group">
                    <label>Contexts</label>
                    <div id="contextList" style="border: 1px solid var(--border); border-radius: 4px;">
                        <div style="padding: 20px; text-align: center; color: var(--text-secondary);">Loading contexts...</div>
                    </div>
                </div>
            </div>
            <div class="modal-footer">
                <button class="toolbar-btn toolbar-btn-secondary" onclick="loadContexts()">↻ Refresh</button>
                <button class="toolbar-btn toolbar-btn-secondary" onclick="hideManageKubeconfigModal()">Close</button>
            </div>
        </div>
    </div>

    <!-- Rename Context Modal -->
    <div class="modal-overlay" id="renameContextModal">
        <div class="modal" style="max-width: 400px;">
            <div class="modal-header">
                <span class="modal-title">Rename Context</span>
                <button class="modal-close" onclick="hideRenameContextModal()">&times;</button>
            </div>
            <div class="modal-body">
                <div class="form-group">
                    <label>Current Name</label>
                    <input type="text" id="renameOldName" readonly style="background: var(--bg-tertiary); cursor: default;">
                </div>
                <div class="form-group">
                    <label>New Name</label>
                    <input type="text" id="renameNewName" placeholder="Enter new context name">
                </div>
            </div>
            <div class="modal-footer">
                <button class="toolbar-btn toolbar-btn-secondary" onclick="hideRenameContextModal()">Cancel</button>
                <button class="toolbar-btn" onclick="confirmRenameContext()">Rename</button>
            </div>
        </div>
    </div>

    <!-- Edit Alias Modal -->
    <div class="modal-overlay" id="editAliasModal">
        <div class="modal" style="max-width: 400px;">
            <div class="modal-header">
                <span class="modal-title">Rename Cluster</span>
                <button class="modal-close" onclick="hideEditAliasModal()">&times;</button>
            </div>
            <div class="modal-body">
                <div class="form-group">
                    <label>Source Name</label>
                    <input type="text" id="aliasClusterName" readonly style="background: var(--bg-tertiary); cursor: default; font-family: monospace; font-size: 12px;">
                    <small style="color: var(--text-secondary); display: block; margin-top: 4px;">Original name from provider (cannot be changed)</small>
                </div>
                <div class="form-group">
                    <label>Display Name</label>
                    <input type="text" id="aliasNewName" placeholder="Enter a friendly name">
                    <small style="color: var(--text-secondary); display: block; margin-top: 4px;">This name will be used in generated kubeconfigs</small>
                </div>
                <input type="hidden" id="aliasClusterId">
                <input type="hidden" id="aliasProfileId">
            </div>
            <div class="modal-footer">
                <button class="toolbar-btn toolbar-btn-secondary" onclick="hideEditAliasModal()">Cancel</button>
                <button class="toolbar-btn" onclick="saveClusterAlias()">Save</button>
            </div>
        </div>
    </div>

    <!-- Toast -->
    <div class="toast" id="toast"></div>

    <script>
        let clusters = [];
        let editingAliasIndex = -1;
        let isConnected = false;
        let profiles = [];
        let selectedProfileId = null;
        let currentProfileType = 'rancher';
        let editingProfileId = null;

        // Get the auth token from meta tag
        function getAuthToken() {
            const meta = document.querySelector('meta[name="auth-token"]');
            return meta ? meta.getAttribute('content') : '';
        }

        // Authenticated fetch wrapper
        async function authFetch(url, options = {}) {
            const token = getAuthToken();
            const headers = options.headers || {};
            if (token) {
                headers['X-Auth-Token'] = token;
            }
            return fetch(url, { ...options, headers });
        }

        function showToast(message, type = 'success') {
            const toast = document.getElementById('toast');
            toast.textContent = message;
            toast.className = 'toast ' + type + ' visible';
            setTimeout(() => toast.classList.remove('visible'), 3000);
        }

        function setLoading(loading) {
            document.getElementById('loadingOverlay').classList.toggle('visible', loading);
        }

        function updateConnectionStatus(connected, text) {
            isConnected = connected;
            document.getElementById('connectionStatus').classList.toggle('connected', connected);
            document.getElementById('connectionStatus').classList.toggle('error', !connected && text.includes('Error'));
            document.getElementById('connectionText').textContent = text;
            document.getElementById('refreshBtn').disabled = !connected;
        }

        function updateSelectionStatus() {
            const selected = getSelectedClusters();
            document.getElementById('selectionStatus').textContent = selected.length + ' selected';
            document.getElementById('generateBtn').disabled = selected.length === 0;
        }

        function isActiveState(state) {
            const s = (state || '').toLowerCase();
            return s === 'active' || s === 'running' || s === 'ready' || s === 'static';
        }

        function getStatusClass(state) {
            const s = (state || '').toLowerCase();
            if (s === 'active' || s === 'running' || s === 'ready' || s === 'static') return 'status-active';
            if (s === 'error' || s === 'unavailable' || s === 'failed') return 'status-error';
            return 'status-inactive';
        }

        function renderClusterTable() {
            const tbody = document.getElementById('clusterTableBody');
            const table = document.getElementById('clusterTable');
            const emptyState = document.getElementById('emptyState');

            tbody.innerHTML = '';

            if (clusters.length === 0) {
                table.classList.add('hidden');
                emptyState.classList.remove('hidden');
                document.getElementById('clusterCount').textContent = '0 clusters';
                return;
            }

            table.classList.remove('hidden');
            emptyState.classList.add('hidden');
            document.getElementById('clusterCount').textContent = clusters.length + ' cluster' + (clusters.length !== 1 ? 's' : '');

            clusters.forEach((cluster, index) => {
                const tr = document.createElement('tr');
                const isActive = isActiveState(cluster.state);
                // Display alias if set, otherwise fall back to the original name
                const displayName = cluster.alias || cluster.name;
                const localBadge = cluster.inLocalKubeconfig ? '<span class="local-config-badge">local</span>' : '';
                tr.innerHTML = ` + "`" + `
                    <td><input type="checkbox" id="cluster-${index}" ${isActive ? 'checked' : ''} ${!isActive ? 'disabled' : ''} onchange="onClusterSelect(${index})"></td>
                    <td class="cluster-name-cell">
                        <div class="cluster-name-row">
                            <span class="cluster-display-name">${escapeHtml(displayName)}</span>
                            ${localBadge}
                            <button class="edit-alias-btn" onclick="showEditAliasModal(${index})" title="Edit name">✏️</button>
                        </div>
                        <div class="cluster-source-name">${escapeHtml(cluster.name)}</div>
                    </td>
                    <td><span class="cluster-source">${escapeHtml(cluster.profileName || '—')}</span></td>
                    <td>${escapeHtml(cluster.provider || '—')}</td>
                    <td><span class="status-badge ${getStatusClass(cluster.state)}">${escapeHtml(cluster.state)}</span></td>
                ` + "`" + `;
                if (isActive) tr.classList.add('selected');
                tbody.appendChild(tr);
            });

            updateSelectAll();
            updateSelectionStatus();
        }

        function escapeHtml(text) {
            const div = document.createElement('div');
            div.textContent = text || '';
            return div.innerHTML;
        }

        function onClusterSelect(index) {
            const checkbox = document.getElementById('cluster-' + index);
            const row = checkbox.closest('tr');
            row.classList.toggle('selected', checkbox.checked);
            updateSelectAll();
            updateSelectionStatus();
        }

        function toggleSelectAll() {
            const selectAll = document.getElementById('selectAll').checked;
            clusters.forEach((cluster, index) => {
                if (isActiveState(cluster.state)) {
                    const checkbox = document.getElementById('cluster-' + index);
                    checkbox.checked = selectAll;
                    checkbox.closest('tr').classList.toggle('selected', selectAll);
                }
            });
            updateSelectionStatus();
        }

        function updateSelectAll() {
            const activeClusters = clusters.filter(c => isActiveState(c.state));
            const checkedCount = activeClusters.filter((_, i) => {
                const idx = clusters.indexOf(activeClusters[i]);
                const cb = document.getElementById('cluster-' + clusters.findIndex(c => c === activeClusters[i]));
                return cb && cb.checked;
            }).length;
            document.getElementById('selectAll').checked = checkedCount === activeClusters.length && activeClusters.length > 0;
        }

        function getSelectedClusters() {
            return clusters.filter((cluster, index) => {
                const cb = document.getElementById('cluster-' + index);
                return cb && cb.checked;
            });
        }

        function toggleOptionsPanel() {
            const panel = document.getElementById('optionsPanel');
            panel.classList.toggle('collapsed');
        }

        function getGenerationOptions() {
            const prefix = document.getElementById('clusterPrefix').value.trim();
            const tagsInput = document.getElementById('aptakubeTags').value.trim();
            const separateFiles = document.getElementById('separateFiles').checked;
            const useSourcePrefix = document.getElementById('useSourcePrefix').checked;
            const mergeToKubeconfig = document.getElementById('mergeToKubeconfig').checked;

            const tags = tagsInput ? tagsInput.split(',').map(t => t.trim()).filter(t => t) : [];

            return { prefix, tags, separateFiles, useSourcePrefix, mergeToKubeconfig };
        }

        async function generateKubeconfig() {
            if (!selectedProfileId) {
                showToast('Please select a connection first', 'error');
                return;
            }

            const selectedClusters = getSelectedClusters();
            if (selectedClusters.length === 0) {
                showToast('Please select at least one cluster', 'error');
                return;
            }

            const options = getGenerationOptions();

            // Cannot merge with separate files option
            if (options.mergeToKubeconfig && options.separateFiles) {
                showToast('Cannot merge when "Generate separate file per source" is enabled', 'error');
                return;
            }

            const btn = document.getElementById('generateBtn');
            btn.disabled = true;
            btn.textContent = options.mergeToKubeconfig ? '⏳ Merging...' : '⏳ Generating...';

            try {
                let response;
                if (selectedProfileId === '__all__') {
                    // Multi-source mode: send cluster info with profile IDs
                    response = await authFetch('/api/generate/all', {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify({
                            selectedClusters: selectedClusters.map(c => ({ id: c.id, name: c.name, alias: c.alias, profileId: c.profileId })),
                            clusterPrefix: options.prefix,
                            aptakubeTags: options.tags,
                            separateFiles: options.separateFiles,
                            useSourcePrefix: options.useSourcePrefix
                        })
                    });
                } else {
                    // Single source mode - send id, name, and alias for each cluster
                    response = await authFetch('/api/generate/profile', {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify({
                            profileId: selectedProfileId,
                            selectedClusters: selectedClusters.map(c => ({ id: c.id, name: c.name, alias: c.alias })),
                            clusterPrefix: options.prefix,
                            aptakubeTags: options.tags,
                            separateFiles: options.separateFiles,
                            useSourcePrefix: options.useSourcePrefix
                        })
                    });
                }

                if (response.headers.get('Content-Type')?.includes('application/json')) {
                    const result = await response.json();
                    showToast(result.error || 'Generation failed', 'error');
                    return;
                }

                const blob = await response.blob();

                // If merge option is enabled, merge to kubeconfig instead of downloading
                if (options.mergeToKubeconfig) {
                    const kubeconfigContent = await blob.text();

                    // Build cluster info for export tracking
                    // The context name is: sourcePrefix (if useSourcePrefix) + clusterPrefix + (alias || name)
                    const clusterInfo = selectedClusters.map(c => {
                        let contextName = '';
                        if (selectedProfileId === '__all__' && options.useSourcePrefix) {
                            // Find the profile name for this cluster
                            const profile = profiles.find(p => p.id === c.profileId);
                            if (profile) {
                                contextName = profile.name.toLowerCase().replace(/[^a-z0-9]/g, '-') + '-';
                            }
                        }
                        contextName += options.prefix + (c.alias || c.name);
                        return {
                            profileId: c.profileId || selectedProfileId,
                            clusterId: c.id,
                            contextName: contextName
                        };
                    });

                    const mergeResponse = await authFetch('/api/context/merge', {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify({
                            kubeconfig: kubeconfigContent,
                            clusters: clusterInfo
                        })
                    });
                    const mergeResult = await mergeResponse.json();
                    if (mergeResult.success) {
                        showToast('Contexts merged into ~/.kube/config successfully!', 'success');
                        // Refresh cluster list to show updated badges
                        if (selectedProfileId === '__all__') {
                            loadAllClusters();
                        } else {
                            loadClustersForProfile(selectedProfileId);
                        }
                    } else {
                        showToast('Failed to merge: ' + mergeResult.error, 'error');
                    }
                    return;
                }

                const url = window.URL.createObjectURL(blob);
                const a = document.createElement('a');
                a.href = url;

                // Use appropriate filename based on content type
                const contentType = response.headers.get('Content-Type');
                if (contentType?.includes('application/zip')) {
                    a.download = 'kubeconfigs.zip';
                } else {
                    a.download = 'kubeconfig.yaml';
                }

                document.body.appendChild(a);
                a.click();
                window.URL.revokeObjectURL(url);
                a.remove();

                showToast('Kubeconfig downloaded successfully!', 'success');

            } catch (error) {
                showToast('Failed to generate: ' + error.message, 'error');
            } finally {
                btn.disabled = false;
                btn.textContent = '⬇ Generate Kubeconfig';
                updateSelectionStatus();
            }
        }

        // Profile Management Functions
        async function loadProfiles() {
            try {
                const response = await authFetch('/api/profiles');
                const result = await response.json();
                if (result.success) {
                    profiles = result.data || [];
                    renderProfileList();
                }
            } catch (error) {
                console.error('Failed to load profiles:', error);
            }
        }

        function renderProfileList() {
            const list = document.getElementById('profileList');
            if (profiles.length === 0) {
                list.innerHTML = '<div class="empty-profiles">No saved connections. Click + to add one.</div>';
                return;
            }

            // Add "All Sources" option at the top
            const allSourcesActive = selectedProfileId === '__all__' ? 'active' : '';
            let html = ` + "`" + `
                <div class="profile-item ${allSourcesActive}" onclick="selectAllSources()">
                    <span class="profile-icon">🌐</span>
                    <div class="profile-info">
                        <div class="profile-name">All Sources</div>
                        <div class="profile-type">${profiles.length} connection${profiles.length !== 1 ? 's' : ''}</div>
                    </div>
                </div>
                <div class="profile-separator"></div>
            ` + "`" + `;

            html += profiles.map(p => {
                const icon = getProfileIcon(p.type);
                const isActive = p.id === selectedProfileId ? 'active' : '';
                return ` + "`" + `
                    <div class="profile-item ${isActive}" onclick="selectProfile('${p.id}')">
                        <span class="profile-icon">${icon}</span>
                        <div class="profile-info">
                            <div class="profile-name">${escapeHtml(p.name)}</div>
                            <div class="profile-type">${p.type}</div>
                        </div>
                        <div class="profile-actions">
                            <button class="profile-action-btn" onclick="event.stopPropagation(); editProfile('${p.id}')" title="Edit">✎</button>
                            <button class="profile-action-btn delete" onclick="event.stopPropagation(); deleteProfile('${p.id}')" title="Delete">&times;</button>
                        </div>
                    </div>
                ` + "`" + `;
            }).join('');

            list.innerHTML = html;
        }

        function getProfileIcon(type) {
            switch (type) {
                case 'rancher': return '🐮';
                case 'eks': return '☁';
                case 'static': return '📄';
                default: return '📁';
            }
        }

        async function selectProfile(profileId) {
            console.log('selectProfile called with:', profileId);
            selectedProfileId = profileId;
            renderProfileList();

            const profile = profiles.find(p => p.id === profileId);
            console.log('Found profile:', profile);
            if (!profile) {
                console.log('Profile not found, returning');
                return;
            }

            setLoading(true);
            updateConnectionStatus(false, 'Loading clusters...');

            // Add timeout to prevent infinite spinning
            const controller = new AbortController();
            const timeoutId = setTimeout(() => controller.abort(), 30000); // 30 second timeout

            try {
                console.log('Fetching clusters for profile:', profileId);
                const response = await authFetch('/api/clusters/profile', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ profileId }),
                    signal: controller.signal
                });
                clearTimeout(timeoutId);
                console.log('Response received:', response.status);
                const result = await response.json();
                console.log('Result:', result);
                if (result.success) {
                    clusters = result.data || [];
                    isConnected = true;
                    updateConnectionStatus(true, 'Connected to ' + profile.name);
                    renderClusterTable();
                    document.getElementById('refreshBtn').disabled = false;
                    document.getElementById('generateBtn').disabled = clusters.length === 0;
                } else {
                    throw new Error(result.error);
                }
            } catch (error) {
                clearTimeout(timeoutId);
                console.error('Error in selectProfile:', error);
                if (error.name === 'AbortError') {
                    showToast('Request timed out - check your connection settings', 'error');
                } else {
                    showToast('Failed to load clusters: ' + error.message, 'error');
                }
                updateConnectionStatus(false, 'Connection failed');
            } finally {
                setLoading(false);
            }
        }

        async function selectAllSources() {
            console.log('selectAllSources called');
            selectedProfileId = '__all__';
            renderProfileList();

            if (profiles.length === 0) {
                showToast('No connections configured', 'error');
                return;
            }

            setLoading(true);
            updateConnectionStatus(false, 'Loading all clusters...');

            const controller = new AbortController();
            const timeoutId = setTimeout(() => controller.abort(), 60000); // 60 second timeout for all sources

            try {
                const response = await authFetch('/api/clusters/all', {
                    method: 'GET',
                    signal: controller.signal
                });
                clearTimeout(timeoutId);
                const result = await response.json();
                if (result.success) {
                    clusters = result.data || [];
                    isConnected = true;
                    updateConnectionStatus(true, 'All Sources (' + clusters.length + ' clusters)');
                    renderClusterTable();
                    document.getElementById('refreshBtn').disabled = false;
                    document.getElementById('generateBtn').disabled = clusters.length === 0;
                } else {
                    throw new Error(result.error);
                }
            } catch (error) {
                clearTimeout(timeoutId);
                console.error('Error in selectAllSources:', error);
                if (error.name === 'AbortError') {
                    showToast('Request timed out', 'error');
                } else {
                    showToast('Failed to load clusters: ' + error.message, 'error');
                }
                updateConnectionStatus(false, 'Connection failed');
            } finally {
                setLoading(false);
            }
        }

        function fetchClusters() {
            // Refresh clusters based on current selection
            if (selectedProfileId === '__all__') {
                selectAllSources();
            } else if (selectedProfileId) {
                selectProfile(selectedProfileId);
            }
        }

        function updatePrefixPreview() {
            const prefix = document.getElementById('clusterPrefix').value;
            const previewEl = document.getElementById('prefixPreview');

            if (!prefix) {
                previewEl.classList.remove('visible');
                return;
            }

            // Get example cluster names from the current clusters
            const selectedClusters = clusters.filter(c => c.selected);
            const exampleClusters = selectedClusters.length > 0 ? selectedClusters : clusters;

            if (exampleClusters.length === 0) {
                previewEl.innerHTML = 'Preview: <span class="prefix-highlight">' + prefix + '</span>example-cluster';
            } else {
                // Show up to 2 examples
                const examples = exampleClusters.slice(0, 2).map(c =>
                    '<span class="prefix-highlight">' + prefix + '</span>' + c.name
                );
                previewEl.innerHTML = 'Preview: ' + examples.join(', ') + (exampleClusters.length > 2 ? '...' : '');
            }

            previewEl.classList.add('visible');
        }

        async function deleteProfile(profileId) {
            if (!confirm('Are you sure you want to delete this connection?')) return;

            try {
                const response = await authFetch('/api/profiles/' + profileId, {
                    method: 'DELETE'
                });
                const result = await response.json();
                if (result.success) {
                    if (selectedProfileId === profileId) {
                        selectedProfileId = null;
                        clusters = [];
                        isConnected = false;
                        updateConnectionStatus(false, 'Not connected');
                        renderClusterTable();
                    }
                    showToast('Connection deleted', 'success');
                    loadProfiles();
                } else {
                    throw new Error(result.error);
                }
            } catch (error) {
                showToast('Failed to delete: ' + error.message, 'error');
            }
        }

        function showAddConnectionModal() {
            editingProfileId = null;
            document.getElementById('modalTitle').textContent = 'Add Connection';
            document.getElementById('addConnectionModal').classList.add('show');
            loadAWSData();
        }

        function hideAddConnectionModal() {
            document.getElementById('addConnectionModal').classList.remove('show');
            editingProfileId = null;
            // Reset form
            document.getElementById('newProfileName').value = '';
            document.getElementById('newRancherUrl').value = '';
            document.getElementById('newRancherToken').value = '';
            document.getElementById('newRancherUsername').value = '';
            document.getElementById('newRancherPassword').value = '';
            document.getElementById('newRancherSkipTls').checked = false;
            document.getElementById('newEksRegion').value = '';
            document.getElementById('newEksProfile').value = '';
            document.getElementById('newEksAccessKey').value = '';
            document.getElementById('newEksSecretKey').value = '';
            document.getElementById('newStaticKubeconfig').value = '';
            currentProfileType = 'rancher';
            document.querySelectorAll('.type-tab').forEach((t, i) => {
                t.classList.toggle('active', i === 0);
            });
            document.querySelectorAll('.type-panel').forEach((p, i) => {
                p.classList.toggle('active', i === 0);
            });
        }

        async function editProfile(profileId) {
            const profile = profiles.find(p => p.id === profileId);
            if (!profile) {
                showToast('Profile not found', 'error');
                return;
            }

            editingProfileId = profileId;
            document.getElementById('modalTitle').textContent = 'Edit Connection';

            // Load AWS data first for EKS profiles
            await loadAWSData();

            // Fill in the form with profile data
            document.getElementById('newProfileName').value = profile.name || '';

            // Set the correct type tab
            currentProfileType = profile.type;
            const typeMap = { 'rancher': 0, 'eks': 1, 'static': 2 };
            const typeIndex = typeMap[profile.type] || 0;
            document.querySelectorAll('.type-tab').forEach((t, i) => {
                t.classList.toggle('active', i === typeIndex);
            });
            document.querySelectorAll('.type-panel').forEach((p, i) => {
                p.classList.toggle('active', i === typeIndex);
            });

            // Fill in type-specific fields
            switch (profile.type) {
                case 'rancher':
                    document.getElementById('newRancherUrl').value = profile.rancherUrl || '';
                    document.getElementById('newRancherToken').value = profile.token || '';
                    document.getElementById('newRancherUsername').value = profile.username || '';
                    document.getElementById('newRancherPassword').value = profile.password || '';
                    document.getElementById('newRancherSkipTls').checked = profile.skipTls || false;
                    break;
                case 'eks':
                    document.getElementById('newEksRegion').value = profile.awsRegion || '';
                    document.getElementById('newEksProfile').value = profile.awsProfile || '';
                    document.getElementById('newEksAccessKey').value = profile.accessKey || '';
                    document.getElementById('newEksSecretKey').value = profile.secretKey || '';
                    break;
                case 'static':
                    document.getElementById('newStaticKubeconfig').value = profile.kubeconfig || '';
                    break;
            }

            document.getElementById('addConnectionModal').classList.add('show');
        }

        function switchProfileType(type, btn) {
            currentProfileType = type;
            document.querySelectorAll('.type-tab').forEach(t => t.classList.remove('active'));
            btn.classList.add('active');

            document.querySelectorAll('.type-panel').forEach(p => p.classList.remove('active'));
            document.getElementById('panel' + type.charAt(0).toUpperCase() + type.slice(1)).classList.add('active');
        }

        async function loadAWSData() {
            try {
                // Load AWS profiles
                const profilesRes = await authFetch('/api/aws/profiles');
                const profilesData = await profilesRes.json();
                if (profilesData.success && profilesData.data) {
                    const select = document.getElementById('newEksProfile');
                    select.innerHTML = '<option value="">Select profile or enter credentials below...</option>' +
                        profilesData.data.map(p => ` + "`" + `<option value="${p}">${p}</option>` + "`" + `).join('');
                }

                // Load AWS regions
                const regionsRes = await authFetch('/api/aws/regions');
                const regionsData = await regionsRes.json();
                if (regionsData.success && regionsData.data) {
                    const select = document.getElementById('newEksRegion');
                    select.innerHTML = '<option value="">Select region...</option>' +
                        regionsData.data.map(r => ` + "`" + `<option value="${r}">${r}</option>` + "`" + `).join('');
                }
            } catch (error) {
                console.error('Failed to load AWS data:', error);
            }
        }

        async function saveNewProfile() {
            const name = document.getElementById('newProfileName').value.trim();
            if (!name) {
                showToast('Please enter a connection name', 'error');
                return;
            }

            let profileData = { name, type: currentProfileType };

            switch (currentProfileType) {
                case 'rancher':
                    profileData.rancherUrl = document.getElementById('newRancherUrl').value.trim();
                    profileData.token = document.getElementById('newRancherToken').value.trim();
                    profileData.username = document.getElementById('newRancherUsername').value.trim();
                    profileData.password = document.getElementById('newRancherPassword').value.trim();
                    profileData.skipTls = document.getElementById('newRancherSkipTls').checked;
                    if (!profileData.rancherUrl) {
                        showToast('Please enter Rancher URL', 'error');
                        return;
                    }
                    const hasToken = profileData.token !== '';
                    const hasUserPass = profileData.username !== '' && profileData.password !== '';
                    if (!hasToken && !hasUserPass) {
                        showToast('Please enter API token or username/password', 'error');
                        return;
                    }
                    break;
                case 'eks':
                    profileData.awsRegion = document.getElementById('newEksRegion').value;
                    profileData.awsProfile = document.getElementById('newEksProfile').value;
                    profileData.accessKey = document.getElementById('newEksAccessKey').value.trim();
                    profileData.secretKey = document.getElementById('newEksSecretKey').value.trim();
                    if (!profileData.awsRegion) {
                        showToast('Please select an AWS region', 'error');
                        return;
                    }
                    if (!profileData.awsProfile && (!profileData.accessKey || !profileData.secretKey)) {
                        showToast('Please select an AWS profile or enter credentials', 'error');
                        return;
                    }
                    break;
                case 'static':
                    profileData.kubeconfig = document.getElementById('newStaticKubeconfig').value.trim();
                    if (!profileData.kubeconfig) {
                        showToast('Please enter kubeconfig content', 'error');
                        return;
                    }
                    break;
            }

            try {
                const isEditing = editingProfileId !== null;
                const url = isEditing ? '/api/profiles/' + editingProfileId : '/api/profiles';
                const method = isEditing ? 'PUT' : 'POST';

                const response = await authFetch(url, {
                    method: method,
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(profileData)
                });
                const result = await response.json();
                if (result.success) {
                    showToast(isEditing ? 'Connection updated!' : 'Connection saved!', 'success');
                    hideAddConnectionModal();
                    loadProfiles();
                    // If we edited the currently selected profile, refresh clusters
                    if (isEditing && selectedProfileId === editingProfileId) {
                        selectProfile(editingProfileId);
                    }
                } else {
                    throw new Error(result.error);
                }
            } catch (error) {
                showToast('Failed to save: ' + error.message, 'error');
            }
        }

        async function loadKubeconfigFromFile() {
            // Check if running in Electron
            if (typeof window.electronAPI === 'undefined' || !window.electronAPI.showOpenDialog) {
                showToast('File loading is only available in the desktop app', 'error');
                return;
            }

            try {
                const result = await window.electronAPI.showOpenDialog({
                    title: 'Select Kubeconfig File',
                    filters: [
                        { name: 'YAML Files', extensions: ['yaml', 'yml'] },
                        { name: 'All Files', extensions: ['*'] }
                    ]
                });

                if (result.canceled || !result.filePaths || result.filePaths.length === 0) {
                    return;
                }

                const filePath = result.filePaths[0];
                const fileResult = await window.electronAPI.readFile(filePath);

                if (!fileResult.success) {
                    showToast('Failed to read file: ' + fileResult.error, 'error');
                    return;
                }

                document.getElementById('newStaticKubeconfig').value = fileResult.content;
                showToast('Kubeconfig loaded from file', 'success');
            } catch (error) {
                showToast('Failed to load file: ' + error.message, 'error');
            }
        }

        // ========== Kubeconfig Context Management ==========

        let kubeconfigContexts = [];

        function showManageKubeconfigModal() {
            document.getElementById('manageKubeconfigModal').classList.add('show');
            loadContexts();
        }

        function hideManageKubeconfigModal() {
            document.getElementById('manageKubeconfigModal').classList.remove('show');
        }

        async function loadContexts() {
            const contextList = document.getElementById('contextList');
            contextList.innerHTML = '<div style="padding: 20px; text-align: center; color: var(--text-secondary);">Loading contexts...</div>';

            try {
                // Get kubeconfig path
                const pathResponse = await authFetch('/api/context');
                const pathResult = await pathResponse.json();
                if (pathResult.success && pathResult.data) {
                    document.getElementById('kubeconfigPath').value = pathResult.data.kubeconfigPath || '~/.kube/config';
                }

                // Get contexts list
                const response = await authFetch('/api/contexts');
                const result = await response.json();

                if (!result.success) {
                    contextList.innerHTML = '<div style="padding: 20px; text-align: center; color: var(--error);">Failed to load contexts: ' + result.error + '</div>';
                    return;
                }

                kubeconfigContexts = result.data || [];

                if (kubeconfigContexts.length === 0) {
                    contextList.innerHTML = '<div style="padding: 20px; text-align: center; color: var(--text-secondary);">No contexts found in kubeconfig</div>';
                    return;
                }

                renderContextList();
            } catch (error) {
                contextList.innerHTML = '<div style="padding: 20px; text-align: center; color: var(--error);">Error: ' + error.message + '</div>';
            }
        }

        function renderContextList() {
            const contextList = document.getElementById('contextList');
            contextList.innerHTML = kubeconfigContexts.map(ctx => ` + "`" + `
                <div style="display: flex; align-items: center; padding: 10px 12px; border-bottom: 1px solid var(--border); gap: 10px;">
                    <div style="flex: 1; min-width: 0;">
                        <div style="display: flex; align-items: center; gap: 8px;">
                            <span style="font-weight: 500; color: var(--text-bright); word-break: break-all;">${ctx.name}</span>
                            ${ctx.isCurrent ? '<span style="background: var(--success); color: white; padding: 2px 6px; border-radius: 3px; font-size: 10px; font-weight: 600;">CURRENT</span>' : ''}
                        </div>
                        <div style="font-size: 11px; color: var(--text-secondary); margin-top: 4px;">
                            <span title="Cluster">📦 ${ctx.cluster}</span>
                            <span style="margin-left: 12px;" title="User">👤 ${ctx.user}</span>
                            ${ctx.namespace ? '<span style="margin-left: 12px;" title="Namespace">📁 ' + ctx.namespace + '</span>' : ''}
                        </div>
                    </div>
                    <div style="display: flex; gap: 4px; flex-shrink: 0;">
                        ${!ctx.isCurrent ? ` + "`" + `<button onclick="setCurrentContext('${ctx.name.replace(/'/g, "\\'")}')" class="toolbar-btn toolbar-btn-secondary" style="padding: 4px 8px; font-size: 11px;" title="Set as current">✓ Use</button>` + "`" + ` : ''}
                        <button onclick="showRenameContextModal('${ctx.name.replace(/'/g, "\\'")}')" class="toolbar-btn toolbar-btn-secondary" style="padding: 4px 8px; font-size: 11px;" title="Rename">✏️</button>
                        <button onclick="deleteContext('${ctx.name.replace(/'/g, "\\'")}')" class="toolbar-btn toolbar-btn-secondary" style="padding: 4px 8px; font-size: 11px; color: var(--error);" title="Delete">🗑️</button>
                    </div>
                </div>
            ` + "`" + `).join('');
        }

        async function setCurrentContext(name) {
            try {
                const response = await authFetch('/api/context', {
                    method: 'PUT',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ context: name })
                });
                const result = await response.json();
                if (result.success) {
                    showToast('Switched to context: ' + name, 'success');
                    loadContexts();
                } else {
                    showToast('Failed to switch context: ' + result.error, 'error');
                }
            } catch (error) {
                showToast('Error: ' + error.message, 'error');
            }
        }

        async function deleteContext(name) {
            if (!confirm('Delete context "' + name + '"?\n\nThis will also remove any orphaned cluster and user entries that are no longer used by other contexts.')) {
                return;
            }

            try {
                const response = await authFetch('/api/context/delete', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ name: name, cleanupOrphans: true })
                });
                const result = await response.json();
                if (result.success) {
                    showToast('Context deleted: ' + name, 'success');
                    loadContexts();
                } else {
                    showToast('Failed to delete context: ' + result.error, 'error');
                }
            } catch (error) {
                showToast('Error: ' + error.message, 'error');
            }
        }

        function showRenameContextModal(oldName) {
            document.getElementById('renameOldName').value = oldName;
            document.getElementById('renameNewName').value = '';
            document.getElementById('renameContextModal').classList.add('show');
            document.getElementById('renameNewName').focus();
        }

        function hideRenameContextModal() {
            document.getElementById('renameContextModal').classList.remove('show');
        }

        async function confirmRenameContext() {
            const oldName = document.getElementById('renameOldName').value;
            const newName = document.getElementById('renameNewName').value.trim();

            if (!newName) {
                showToast('Please enter a new name', 'error');
                return;
            }

            if (oldName === newName) {
                hideRenameContextModal();
                return;
            }

            try {
                const response = await authFetch('/api/context/rename', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ oldName: oldName, newName: newName })
                });
                const result = await response.json();
                if (result.success) {
                    showToast('Context renamed: ' + oldName + ' → ' + newName, 'success');
                    hideRenameContextModal();
                    loadContexts();
                } else {
                    showToast('Failed to rename: ' + result.error, 'error');
                }
            } catch (error) {
                showToast('Error: ' + error.message, 'error');
            }
        }

        // Cluster alias modal functions
        function showEditAliasModal(index) {
            if (index < 0 || index >= clusters.length) {
                showToast('Invalid cluster index', 'error');
                return;
            }

            const cluster = clusters[index];
            editingAliasIndex = index;

            document.getElementById('aliasClusterName').value = cluster.name;
            // Pre-populate with alias if set, otherwise use the original name
            document.getElementById('aliasNewName').value = cluster.alias || cluster.name;
            document.getElementById('aliasClusterId').value = cluster.id;
            document.getElementById('aliasProfileId').value = cluster.profileId;

            document.getElementById('editAliasModal').classList.add('show');
            const input = document.getElementById('aliasNewName');
            input.focus();
            input.select(); // Select all text so user can easily type over it
        }

        function hideEditAliasModal() {
            document.getElementById('editAliasModal').classList.remove('show');
            editingAliasIndex = -1;
        }

        async function saveClusterAlias() {
            const clusterId = document.getElementById('aliasClusterId').value;
            const profileId = document.getElementById('aliasProfileId').value;
            const sourceName = document.getElementById('aliasClusterName').value;
            let displayName = document.getElementById('aliasNewName').value.trim();

            if (!clusterId || !profileId) {
                showToast('Missing cluster or profile information', 'error');
                return;
            }

            // If display name matches source name, clear the alias (use default)
            const alias = (displayName === sourceName) ? '' : displayName;

            try {
                const response = await authFetch('/api/cluster/alias', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ profileId: profileId, clusterId: clusterId, alias: alias })
                });
                const result = await response.json();
                if (result.success) {
                    if (alias) {
                        showToast('Renamed: ' + displayName, 'success');
                    } else {
                        showToast('Reset to source name', 'success');
                    }
                    hideEditAliasModal();
                    // Refresh the cluster list to show updated alias
                    fetchClusters();
                } else {
                    showToast('Failed to save: ' + result.error, 'error');
                }
            } catch (error) {
                showToast('Error: ' + error.message, 'error');
            }
        }

        // Load profiles on page load
        document.addEventListener('DOMContentLoaded', loadProfiles);
    </script>
</body>
</html>`
