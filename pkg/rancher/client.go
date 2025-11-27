// Package rancher provides a client for interacting with the Rancher API
package rancher

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

	"github.com/rancher-kubeconfig-proxy/pkg/config"
)

// Client is a Rancher API client
type Client struct {
	config      *config.Config
	httpClient  *http.Client
	bearerToken string // Used for password auth after login
}

// LoginRequest represents the request body for password authentication
type LoginRequest struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	ResponseType string `json:"responseType"`
}

// LoginResponse represents the response from the login endpoint
type LoginResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Token       string `json:"token"`
	UserID      string `json:"userId"`
	Description string `json:"description"`
}

// Cluster represents a Rancher managed cluster
type Cluster struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	State       string `json:"state"`
	Provider    string `json:"provider"`
	Links       struct {
		Self             string `json:"self"`
		GenerateKubeconfig string `json:"generateKubeconfig"`
	} `json:"links"`
	Actions struct {
		GenerateKubeconfig string `json:"generateKubeconfig"`
	} `json:"actions"`
}

// ClusterCollection represents the response from the clusters endpoint
type ClusterCollection struct {
	Data []Cluster `json:"data"`
}

// KubeconfigResponse represents the response from generateKubeconfig action
type KubeconfigResponse struct {
	Config string `json:"config"`
}

// NewClient creates a new Rancher API client
func NewClient(cfg *config.Config) (*Client, error) {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: cfg.InsecureSkipTLSVerify,
	}

	// Load custom CA certificate if provided
	if cfg.CACert != "" {
		caCert, err := os.ReadFile(cfg.CACert)
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

	client := &Client{
		config:     cfg,
		httpClient: httpClient,
	}

	// If using password auth, perform login to get a bearer token
	if cfg.UsePasswordAuth() {
		if err := client.login(); err != nil {
			return nil, fmt.Errorf("failed to authenticate with username/password: %w", err)
		}
	}

	return client, nil
}

// login authenticates with username/password and stores the bearer token
func (c *Client) login() error {
	loginReq := LoginRequest{
		Username:     c.config.Username,
		Password:     c.config.Password,
		ResponseType: "token",
	}

	body, err := json.Marshal(loginReq)
	if err != nil {
		return fmt.Errorf("failed to marshal login request: %w", err)
	}

	// Try local authentication first
	url := fmt.Sprintf("%s/v3-public/localProviders/local?action=login", c.config.RancherURL)

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("login failed: status %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	var loginResp LoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&loginResp); err != nil {
		return fmt.Errorf("failed to decode login response: %w", err)
	}

	if loginResp.Token == "" {
		return fmt.Errorf("login succeeded but no token was returned")
	}

	c.bearerToken = loginResp.Token
	return nil
}

// doRequest performs an HTTP request with authentication
func (c *Client) doRequest(method, url string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Use bearer token if we authenticated with password, otherwise use basic auth
	if c.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearerToken)
	} else {
		username, password := c.config.GetBasicAuth()
		req.SetBasicAuth(username, password)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	return resp, nil
}

// ListClusters retrieves all clusters from the Rancher API
func (c *Client) ListClusters() ([]Cluster, error) {
	url := fmt.Sprintf("%s/v3/clusters", c.config.RancherURL)

	resp, err := c.doRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to list clusters: status %d, body: %s", resp.StatusCode, string(bodyBytes))
	}

	var collection ClusterCollection
	if err := json.NewDecoder(resp.Body).Decode(&collection); err != nil {
		return nil, fmt.Errorf("failed to decode clusters response: %w", err)
	}

	return collection.Data, nil
}

// GetClusterKubeconfig retrieves the kubeconfig for a specific cluster
func (c *Client) GetClusterKubeconfig(cluster *Cluster) (string, error) {
	// Use the generateKubeconfig action URL from the cluster
	url := cluster.Actions.GenerateKubeconfig
	if url == "" {
		// Fall back to constructing the URL manually
		url = fmt.Sprintf("%s/v3/clusters/%s?action=generateKubeconfig", c.config.RancherURL, cluster.ID)
	}

	resp, err := c.doRequest("POST", url, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to get kubeconfig for cluster %s: status %d, body: %s",
			cluster.Name, resp.StatusCode, string(bodyBytes))
	}

	var kubeconfigResp KubeconfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&kubeconfigResp); err != nil {
		return "", fmt.Errorf("failed to decode kubeconfig response: %w", err)
	}

	return kubeconfigResp.Config, nil
}

// GetAllKubeconfigs retrieves kubeconfigs for all active clusters
func (c *Client) GetAllKubeconfigs() (map[string]string, error) {
	clusters, err := c.ListClusters()
	if err != nil {
		return nil, err
	}

	kubeconfigs := make(map[string]string)
	for _, cluster := range clusters {
		// Skip clusters that are not active
		if cluster.State != "active" {
			continue
		}

		kubeconfig, err := c.GetClusterKubeconfig(&cluster)
		if err != nil {
			// Log the error but continue with other clusters
			fmt.Fprintf(os.Stderr, "Warning: failed to get kubeconfig for cluster %s: %v\n", cluster.Name, err)
			continue
		}

		kubeconfigs[cluster.Name] = kubeconfig
	}

	return kubeconfigs, nil
}
