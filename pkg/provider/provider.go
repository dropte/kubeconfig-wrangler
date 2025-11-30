// Package provider defines the interface for cluster providers (Rancher, EKS, etc.)
package provider

// ClusterInfo represents a Kubernetes cluster from any provider
type ClusterInfo struct {
	// ID is the unique identifier for the cluster
	ID string `json:"id"`

	// Name is the display name of the cluster
	Name string `json:"name"`

	// State is the current state of the cluster (e.g., "active", "ACTIVE", "provisioning")
	State string `json:"state"`

	// Provider is the source provider type ("rancher" or "eks")
	Provider string `json:"provider"`

	// ProfileID is the ID of the profile this cluster belongs to
	ProfileID string `json:"profileId"`

	// ProfileName is the name of the profile this cluster belongs to
	ProfileName string `json:"profileName"`

	// Region is the cloud region (primarily for EKS)
	Region string `json:"region,omitempty"`

	// Description is an optional description of the cluster
	Description string `json:"description,omitempty"`

	// Alias is a user-defined friendly name for this cluster (used in generated kubeconfigs)
	Alias string `json:"alias,omitempty"`
}

// ClusterProvider is the interface that all cluster providers must implement
type ClusterProvider interface {
	// Name returns the display name of this provider instance
	Name() string

	// Type returns the provider type ("rancher" or "eks")
	Type() string

	// ProfileID returns the ID of the profile associated with this provider
	ProfileID() string

	// ListClusters retrieves all available clusters from this provider
	ListClusters() ([]ClusterInfo, error)

	// GetKubeconfig retrieves the kubeconfig for a specific cluster
	GetKubeconfig(clusterID string) (string, error)

	// Close cleans up any resources held by the provider
	Close() error
}

// Registry holds all active provider instances
type Registry struct {
	providers map[string]ClusterProvider
}

// NewRegistry creates a new provider registry
func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]ClusterProvider),
	}
}

// Register adds a provider to the registry
func (r *Registry) Register(provider ClusterProvider) {
	r.providers[provider.ProfileID()] = provider
}

// Unregister removes a provider from the registry
func (r *Registry) Unregister(profileID string) {
	if p, exists := r.providers[profileID]; exists {
		p.Close()
		delete(r.providers, profileID)
	}
}

// Get retrieves a provider by profile ID
func (r *Registry) Get(profileID string) (ClusterProvider, bool) {
	p, exists := r.providers[profileID]
	return p, exists
}

// All returns all registered providers
func (r *Registry) All() []ClusterProvider {
	result := make([]ClusterProvider, 0, len(r.providers))
	for _, p := range r.providers {
		result = append(result, p)
	}
	return result
}

// ListAllClusters retrieves clusters from all registered providers
func (r *Registry) ListAllClusters() ([]ClusterInfo, error) {
	var allClusters []ClusterInfo
	for _, p := range r.providers {
		clusters, err := p.ListClusters()
		if err != nil {
			// Log error but continue with other providers
			continue
		}
		allClusters = append(allClusters, clusters...)
	}
	return allClusters, nil
}

// Close cleans up all providers
func (r *Registry) Close() {
	for _, p := range r.providers {
		p.Close()
	}
	r.providers = make(map[string]ClusterProvider)
}
