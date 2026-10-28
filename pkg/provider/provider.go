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

	// InLocalKubeconfig indicates whether this cluster already exists in the user's ~/.kube/config
	InLocalKubeconfig bool `json:"inLocalKubeconfig"`
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
