// Package profile provides data structures and persistence for connection profiles
package profile

import (
	"maps"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"
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
	// ProfileTypeOKE represents an Oracle Cloud (OKE) connection profile
	ProfileTypeOKE ProfileType = "oke"
)

// OCI authentication modes for OKE profiles.
const (
	// OCIAuthAPIKey signs requests with the API key from the OCI config profile.
	OCIAuthAPIKey = "api_key"
	// OCIAuthSecurityToken uses a session token created by "oci session authenticate".
	OCIAuthSecurityToken = "security_token"
)

// OKE kubeconfig endpoint kinds (which control-plane endpoint the kubeconfig targets).
const (
	OKEEndpointPublic      = "public"
	OKEEndpointPrivate     = "private"
	OKEEndpointVCNHostname = "vcn_hostname"
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

	// OKE (Oracle Cloud) fields. No secrets are stored: authentication always
	// goes through a profile in the OCI config file.

	// OCIProfile is the profile name in the OCI config file (default DEFAULT)
	OCIProfile string `json:"ociProfile,omitempty"`

	// OCIConfigFile overrides the OCI config file path (default ~/.oci/config)
	OCIConfigFile string `json:"ociConfigFile,omitempty"`

	// OCIRegion overrides the region from the OCI profile (optional)
	OCIRegion string `json:"ociRegion,omitempty"`

	// OCICompartment restricts discovery to one compartment OCID; empty means
	// every active compartment in the tenancy
	OCICompartment string `json:"ociCompartment,omitempty"`

	// OCIAuth is the OCI auth mode: OCIAuthAPIKey (default) or OCIAuthSecurityToken
	OCIAuth string `json:"ociAuth,omitempty"`

	// OKEEndpoint selects the cluster endpoint written into kubeconfigs:
	// OKEEndpointPublic (default), OKEEndpointPrivate or OKEEndpointVCNHostname
	OKEEndpoint string `json:"okeEndpoint,omitempty"`

	// ClusterAliases maps cluster IDs to user-defined friendly names
	// The alias is used instead of the original cluster name in generated kubeconfigs
	ClusterAliases map[string]string `json:"clusterAliases,omitempty"`

	// ExportedContexts maps cluster IDs to the context name used when merging to ~/.kube/config
	// This tracks which clusters have been exported and with what context name
	ExportedContexts map[string]string `json:"exportedContexts,omitempty"`

	// Locked is set (in memory only) on profiles whose secrets could not be
	// decrypted. Their secret fields are blank; re-entering them via Update
	// unlocks the profile.
	Locked bool `json:"-"`
}

// clone returns a deep copy of p so callers never share the store's maps.
func (p *Profile) clone() *Profile {
	cp := *p
	cp.ClusterAliases = maps.Clone(p.ClusterAliases)
	cp.ExportedContexts = maps.Clone(p.ExportedContexts)
	return &cp
}

// hasSecrets reports whether any secret (encrypted) field is set.
func hasSecrets(p *Profile) bool {
	return p.Token != "" || p.Password != "" || p.SecretKey != "" ||
		p.SessionToken != "" || p.Kubeconfig != ""
}

// ProfileView is the secret-free representation of a profile sent to clients.
type ProfileView struct {
	Id                 string            `json:"id"`
	Name               string            `json:"name"`
	Type               ProfileType       `json:"type"`
	CreatedAt          time.Time         `json:"createdAt"`
	UpdatedAt          time.Time         `json:"updatedAt"`
	RancherURL         string            `json:"rancherUrl,omitempty"`
	Username           string            `json:"username,omitempty"`
	SkipTLS            bool              `json:"skipTls"`
	CACert             string            `json:"caCert,omitempty"`
	AWSProfile         string            `json:"awsProfile,omitempty"`
	AWSRegion          string            `json:"awsRegion,omitempty"`
	AccessKey          string            `json:"accessKey,omitempty"`
	OCIProfile         string            `json:"ociProfile,omitempty"`
	OCIConfigFile      string            `json:"ociConfigFile,omitempty"`
	OCIRegion          string            `json:"ociRegion,omitempty"`
	OCICompartment     string            `json:"ociCompartment,omitempty"`
	OCIAuth            string            `json:"ociAuth,omitempty"`
	OKEEndpoint        string            `json:"okeEndpoint,omitempty"`
	ClusterAliases     map[string]string `json:"clusterAliases,omitempty"`
	HasToken           bool              `json:"hasToken"`
	HasPassword        bool              `json:"hasPassword"`
	HasSecretKey       bool              `json:"hasSecretKey"`
	HasSessionToken    bool              `json:"hasSessionToken"`
	HasKubeconfig      bool              `json:"hasKubeconfig"`
	KubeconfigContexts int               `json:"kubeconfigContexts"`
	Locked             bool              `json:"locked"`
}

// View returns the client-facing view of p with all secrets replaced by
// presence flags.
func (p *Profile) View() ProfileView {
	v := ProfileView{
		Id:              p.ID,
		Name:            p.Name,
		Type:            p.Type,
		CreatedAt:       p.CreatedAt,
		UpdatedAt:       p.UpdatedAt,
		RancherURL:      p.RancherURL,
		Username:        p.Username,
		SkipTLS:         p.SkipTLS,
		CACert:          p.CACert,
		AWSProfile:      p.AWSProfile,
		AWSRegion:       p.AWSRegion,
		AccessKey:       p.AccessKey,
		OCIProfile:      p.OCIProfile,
		OCIConfigFile:   p.OCIConfigFile,
		OCIRegion:       p.OCIRegion,
		OCICompartment:  p.OCICompartment,
		OCIAuth:         p.OCIAuth,
		OKEEndpoint:     p.OKEEndpoint,
		ClusterAliases:  maps.Clone(p.ClusterAliases),
		HasToken:        p.Token != "",
		HasPassword:     p.Password != "",
		HasSecretKey:    p.SecretKey != "",
		HasSessionToken: p.SessionToken != "",
		HasKubeconfig:   p.Kubeconfig != "",
		Locked:          p.Locked,
	}
	if p.Kubeconfig != "" {
		if cfg, err := clientcmd.Load([]byte(p.Kubeconfig)); err == nil {
			v.KubeconfigContexts = len(cfg.Contexts)
		}
	}
	return v
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

// IsOKE returns true if this is an Oracle Cloud (OKE) profile
func (p *Profile) IsOKE() bool {
	return p.Type == ProfileTypeOKE
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
	case ProfileTypeOKE:
		return p.validateOKE()
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

func (p *Profile) validateOKE() error {
	switch p.OCIAuth {
	case "", OCIAuthAPIKey, OCIAuthSecurityToken:
	default:
		return &ValidationError{Field: "ociAuth", Message: "OCI auth must be api_key or security_token"}
	}
	switch p.OKEEndpoint {
	case "", OKEEndpointPublic, OKEEndpointPrivate, OKEEndpointVCNHostname:
	default:
		return &ValidationError{Field: "okeEndpoint", Message: "OKE endpoint must be public, private or vcn_hostname"}
	}
	if p.OCICompartment != "" && !strings.HasPrefix(p.OCICompartment, "ocid1.") {
		return &ValidationError{Field: "ociCompartment", Message: "compartment must be an OCID (ocid1....)"}
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

	// OKE fields
	OCIProfile     string `json:"ociProfile,omitempty"`
	OCIConfigFile  string `json:"ociConfigFile,omitempty"`
	OCIRegion      string `json:"ociRegion,omitempty"`
	OCICompartment string `json:"ociCompartment,omitempty"`
	OCIAuth        string `json:"ociAuth,omitempty"`
	OKEEndpoint    string `json:"okeEndpoint,omitempty"`

	// ClusterAliases maps cluster IDs to user-defined friendly names
	ClusterAliases map[string]string `json:"clusterAliases,omitempty"`
}

// ToProfile converts a create request to a Profile. Fields that do not
// belong to the requested type are dropped so, for example, an OKE profile
// (which authenticates only through the OCI config file) never stores
// secrets a client sent by mistake.
func (r *ProfileCreateRequest) ToProfile(id string) *Profile {
	now := time.Now()
	if r.Type == ProfileTypeOKE {
		r.Token, r.Password, r.SecretKey, r.SessionToken, r.Kubeconfig = "", "", "", "", ""
	}
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
		OCIProfile:     strings.TrimSpace(r.OCIProfile),
		OCIConfigFile:  strings.TrimSpace(r.OCIConfigFile),
		OCIRegion:      strings.TrimSpace(r.OCIRegion),
		OCICompartment: strings.TrimSpace(r.OCICompartment),
		OCIAuth:        r.OCIAuth,
		OKEEndpoint:    r.OKEEndpoint,
		ClusterAliases: r.ClusterAliases,
	}
}
