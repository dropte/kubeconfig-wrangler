package provider

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// RancherConfig holds the configuration for a Rancher provider
type RancherConfig struct {
	// ProfileID is the unique identifier for this profile
	ProfileID string

	// ProfileName is the display name for this profile
	ProfileName string

	// URL is the Rancher server URL
	URL string

	// Token is the API token (access_key:secret_key format)
	Token string

	// Username for password-based authentication
	Username string

	// Password for password-based authentication
	Password string

	// SkipTLSVerify skips TLS certificate verification
	SkipTLSVerify bool

	// CACert is the path to a custom CA certificate
	CACert string
}

// RancherProvider implements ClusterProvider for Rancher
type RancherProvider struct {
	config      RancherConfig
	httpClient  *http.Client
	bearerToken string
	accessKey   string
	secretKey   string
}

// loginRequest represents the request body for password authentication
type loginRequest struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	ResponseType string `json:"responseType"`
}

// loginResponse represents the response from the login endpoint
type loginResponse struct {
	Token string `json:"token"`
}

// rancherCluster represents a Rancher managed cluster
type rancherCluster struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	State       string `json:"state"`
	Provider    string `json:"provider"`
	Actions     struct {
		GenerateKubeconfig string `json:"generateKubeconfig"`
	} `json:"actions"`
}

// clusterCollection represents the response from the clusters endpoint
type clusterCollection struct {
	Data []rancherCluster `json:"data"`
}

// kubeconfigResponse represents the response from generateKubeconfig action
type kubeconfigResponse struct {
	Config string `json:"config"`
}

// NewRancherProvider creates a new Rancher provider instance
func NewRancherProvider(config RancherConfig) (*RancherProvider, error) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: config.SkipTLSVerify,
	}

	// Load custom CA certificate if provided
	if config.CACert != "" {
		caCert, err := os.ReadFile(config.CACert)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate: %w", err)
		}
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		tlsConfig.RootCAs = caCertPool
	}

	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	provider := &RancherProvider{
		config:     config,
		httpClient: httpClient,
	}

	// Determine authentication method and authenticate
	if config.Username != "" && config.Password != "" {
		if err := provider.login(); err != nil {
			return nil, fmt.Errorf("failed to authenticate with username/password: %w", err)
		}
	} else if config.Token != "" {
		// Parse token into access_key:secret_key
		provider.accessKey, provider.secretKey = parseToken(config.Token)
		if provider.accessKey == "" || provider.secretKey == "" {
			return nil, fmt.Errorf("invalid token format, expected 'access_key:secret_key'")
		}
	} else {
		return nil, fmt.Errorf("authentication required: provide either token or username+password")
	}

	return provider, nil
}

// parseToken splits a token into access key and secret key
func parseToken(token string) (accessKey, secretKey string) {
	for i := 0; i < len(token); i++ {
		if token[i] == ':' {
			return token[:i], token[i+1:]
		}
	}
	return "", ""
}

// login authenticates with username/password and stores the bearer token
func (p *RancherProvider) login() error {
	loginReq := loginRequest{
		Username:     p.config.Username,
		Password:     p.config.Password,
		ResponseType: "token",
	}

	body, err := json.Marshal(loginReq)
	if err != nil {
		return fmt.Errorf("failed to marshal login request: %w", err)
	}

	url := fmt.Sprintf("%s/v3-public/localProviders/local?action=login", p.config.URL)

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("login failed: status %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	var loginResp loginResponse
	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		return fmt.Errorf("failed to decode login response: %w", err)
	}

	if loginResp.Token == "" {
		return fmt.Errorf("login succeeded but no token was returned")
	}

	p.bearerToken = loginResp.Token
	return nil
}

// doRequest performs an HTTP request with authentication
func (p *RancherProvider) doRequest(method, url string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Use bearer token if we authenticated with password, otherwise use basic auth
	if p.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+p.bearerToken)
	} else {
		req.SetBasicAuth(p.accessKey, p.secretKey)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	return resp, nil
}

// Name returns the display name of this provider instance
func (p *RancherProvider) Name() string {
	return p.config.ProfileName
}

// Type returns the provider type
func (p *RancherProvider) Type() string {
	return "rancher"
}

// ProfileID returns the profile ID
func (p *RancherProvider) ProfileID() string {
	return p.config.ProfileID
}

// ListClusters retrieves all clusters from this Rancher instance
func (p *RancherProvider) ListClusters() ([]ClusterInfo, error) {
	url := fmt.Sprintf("%s/v3/clusters", p.config.URL)

	resp, err := p.doRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to list clusters: status %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	var collection clusterCollection
	if err := json.NewDecoder(resp.Body).Decode(&collection); err != nil {
		return nil, fmt.Errorf("failed to decode clusters response: %w", err)
	}

	clusters := make([]ClusterInfo, len(collection.Data))
	for i, c := range collection.Data {
		clusters[i] = ClusterInfo{
			ID:          c.ID,
			Name:        c.Name,
			State:       c.State,
			Provider:    "rancher",
			ProfileID:   p.config.ProfileID,
			ProfileName: p.config.ProfileName,
			Description: c.Description,
		}
	}

	return clusters, nil
}

// GetKubeconfig retrieves the kubeconfig for a specific cluster
func (p *RancherProvider) GetKubeconfig(clusterID string) (string, error) {
	url := fmt.Sprintf("%s/v3/clusters/%s?action=generateKubeconfig", p.config.URL, clusterID)

	resp, err := p.doRequest("POST", url, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to get kubeconfig for cluster %s: status %d, body: %s",
			clusterID, resp.StatusCode, string(bodyBytes))
	}

	var kubeconfigResp kubeconfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&kubeconfigResp); err != nil {
		return "", fmt.Errorf("failed to decode kubeconfig response: %w", err)
	}

	return kubeconfigResp.Config, nil
}

// Close cleans up any resources held by the provider
func (p *RancherProvider) Close() error {
	// No persistent resources to clean up
	return nil
}
