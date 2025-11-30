// Package profile provides data structures and persistence for connection profiles
package profile

import (
	"time"
)

// ProfileType represents the type of cluster provider
type ProfileType string

const (
	// ProfileTypeRancher represents a Rancher connection profile
	ProfileTypeRancher ProfileType = "rancher"
	// ProfileTypeEKS represents an AWS EKS connection profile
	ProfileTypeEKS ProfileType = "eks"
	// ProfileTypeStatic represents a manually entered kubeconfig
	ProfileTypeStatic ProfileType = "static"
)

// Profile represents a saved connection configuration
type Profile struct {
	// ID is the unique identifier for this profile
	ID string `json:"id"`

	// Name is the display name for this profile
	Name string `json:"name"`

	// Type is the profile type (rancher or eks)
	Type ProfileType `json:"type"`

	// CreatedAt is when this profile was created
	CreatedAt time.Time `json:"createdAt"`

	// UpdatedAt is when this profile was last updated
	UpdatedAt time.Time `json:"updatedAt"`

	// Rancher-specific fields

	// RancherURL is the Rancher server URL
	RancherURL string `json:"rancherUrl,omitempty"`

	// Token is the Rancher API token (access_key:secret_key format)
	Token string `json:"token,omitempty"`

	// Username for password-based authentication
	Username string `json:"username,omitempty"`

	// Password for password-based authentication (stored encrypted)
	Password string `json:"password,omitempty"`

	// SkipTLS skips TLS certificate verification
	SkipTLS bool `json:"skipTls,omitempty"`

	// CACert is the path to a custom CA certificate
	CACert string `json:"caCert,omitempty"`

	// EKS-specific fields

	// AWSProfile is the name of the AWS CLI profile to use
	AWSProfile string `json:"awsProfile,omitempty"`

	// AWSRegion is the AWS region for EKS clusters
	AWSRegion string `json:"awsRegion,omitempty"`

	// AccessKey is the AWS access key ID (for direct credential entry)
	AccessKey string `json:"accessKey,omitempty"`

	// SecretKey is the AWS secret access key (stored encrypted)
	SecretKey string `json:"secretKey,omitempty"`

	// SessionToken is an optional AWS session token (stored encrypted)
	SessionToken string `json:"sessionToken,omitempty"`

	// Static kubeconfig fields

	// Kubeconfig is the raw kubeconfig content (stored encrypted)
	Kubeconfig string `json:"kubeconfig,omitempty"`

	// ClusterAliases maps cluster IDs to user-defined friendly names
	// The alias is used instead of the original cluster name in generated kubeconfigs
	ClusterAliases map[string]string `json:"clusterAliases,omitempty"`
}

// IsRancher returns true if this is a Rancher profile
func (p *Profile) IsRancher() bool {
	return p.Type == ProfileTypeRancher
}

// IsEKS returns true if this is an EKS profile
func (p *Profile) IsEKS() bool {
	return p.Type == ProfileTypeEKS
}

// IsStatic returns true if this is a static kubeconfig profile
func (p *Profile) IsStatic() bool {
	return p.Type == ProfileTypeStatic
}

// UsesAWSProfile returns true if this EKS profile uses an AWS CLI profile
func (p *Profile) UsesAWSProfile() bool {
	return p.Type == ProfileTypeEKS && p.AWSProfile != ""
}

// UsesDirectCredentials returns true if this EKS profile uses direct AWS credentials
func (p *Profile) UsesDirectCredentials() bool {
	return p.Type == ProfileTypeEKS && p.AccessKey != "" && p.SecretKey != ""
}

// Validate checks if the profile has all required fields
func (p *Profile) Validate() error {
	if p.Name == "" {
		return &ValidationError{Field: "name", Message: "name is required"}
	}

	switch p.Type {
	case ProfileTypeRancher:
		return p.validateRancher()
	case ProfileTypeEKS:
		return p.validateEKS()
	case ProfileTypeStatic:
		return p.validateStatic()
	default:
		return &ValidationError{Field: "type", Message: "invalid profile type"}
	}
}

func (p *Profile) validateRancher() error {
	if p.RancherURL == "" {
		return &ValidationError{Field: "rancherUrl", Message: "Rancher URL is required"}
	}

	hasToken := p.Token != ""
	hasPassword := p.Username != "" && p.Password != ""

	if !hasToken && !hasPassword {
		return &ValidationError{Field: "auth", Message: "token or username/password is required"}
	}

	return nil
}

func (p *Profile) validateEKS() error {
	if p.AWSRegion == "" {
		return &ValidationError{Field: "awsRegion", Message: "AWS region is required"}
	}

	hasProfile := p.AWSProfile != ""
	hasCreds := p.AccessKey != "" && p.SecretKey != ""

	if !hasProfile && !hasCreds {
		return &ValidationError{Field: "auth", Message: "AWS profile or access credentials required"}
	}

	return nil
}

func (p *Profile) validateStatic() error {
	if p.Kubeconfig == "" {
		return &ValidationError{Field: "kubeconfig", Message: "kubeconfig content is required"}
	}
	return nil
}

// ValidationError represents a profile validation error
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Field + ": " + e.Message
}

// ProfileCreateRequest represents a request to create a new profile
type ProfileCreateRequest struct {
	Name string      `json:"name"`
	Type ProfileType `json:"type"`

	// Rancher fields
	RancherURL string `json:"rancherUrl,omitempty"`
	Token      string `json:"token,omitempty"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	SkipTLS    bool   `json:"skipTls,omitempty"`
	CACert     string `json:"caCert,omitempty"`

	// EKS fields
	AWSProfile   string `json:"awsProfile,omitempty"`
	AWSRegion    string `json:"awsRegion,omitempty"`
	AccessKey    string `json:"accessKey,omitempty"`
	SecretKey    string `json:"secretKey,omitempty"`
	SessionToken string `json:"sessionToken,omitempty"`

	// Static kubeconfig fields
	Kubeconfig string `json:"kubeconfig,omitempty"`

	// ClusterAliases maps cluster IDs to user-defined friendly names
	ClusterAliases map[string]string `json:"clusterAliases,omitempty"`
}

// ToProfile converts a create request to a Profile
func (r *ProfileCreateRequest) ToProfile(id string) *Profile {
	now := time.Now()
	return &Profile{
		ID:             id,
		Name:           r.Name,
		Type:           r.Type,
		CreatedAt:      now,
		UpdatedAt:      now,
		RancherURL:     r.RancherURL,
		Token:          r.Token,
		Username:       r.Username,
		Password:       r.Password,
		SkipTLS:        r.SkipTLS,
		CACert:         r.CACert,
		AWSProfile:     r.AWSProfile,
		AWSRegion:      r.AWSRegion,
		AccessKey:      r.AccessKey,
		SecretKey:      r.SecretKey,
		SessionToken:   r.SessionToken,
		Kubeconfig:     r.Kubeconfig,
		ClusterAliases: r.ClusterAliases,
	}
}
