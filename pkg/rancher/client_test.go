package rancher

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher-kubeconfig-proxy/pkg/config"
)

// Sample kubeconfig for testing
const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://test-cluster.example.com:6443
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-cluster
current-context: test-cluster
users:
- name: test-user
  user:
    token: test-token-12345
`

func TestNewClient_BasicAuth(t *testing.T) {
	cfg := &config.Config{
		RancherURL: "https://rancher.example.com",
		AccessKey:  "access123",
		SecretKey:  "secret456",
		AuthMethod: config.AuthMethodToken,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("unexpected error creating client: %v", err)
	}

	if client == nil {
		t.Fatal("expected client to be non-nil")
	}
	if client.config != cfg {
		t.Error("client config not set correctly")
	}
	if client.bearerToken != "" {
		t.Error("bearer token should be empty for basic auth")
	}
}

func TestNewClient_PasswordAuth(t *testing.T) {
	// Create a test server that handles login
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3-public/localProviders/local" && r.URL.Query().Get("action") == "login" {
			// Verify request
			var loginReq LoginRequest
			if err := json.NewDecoder(r.Body).Decode(&loginReq); err != nil {
				t.Errorf("failed to decode login request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			if loginReq.Username != "admin" || loginReq.Password != "password123" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

			// Return successful login response
			resp := LoginResponse{
				ID:     "token-12345",
				Token:  "kubeconfig-token:secret-value",
				UserID: "user-abc",
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cfg := &config.Config{
		RancherURL: server.URL,
		Username:   "admin",
		Password:   "password123",
		AuthMethod: config.AuthMethodPassword,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("unexpected error creating client: %v", err)
	}

	if client.bearerToken != "kubeconfig-token:secret-value" {
		t.Errorf("bearer token = %q, want %q", client.bearerToken, "kubeconfig-token:secret-value")
	}
}

func TestNewClient_PasswordAuthFailure(t *testing.T) {
	// Create a test server that rejects login
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message": "invalid credentials"}`))
	}))
	defer server.Close()

	cfg := &config.Config{
		RancherURL: server.URL,
		Username:   "admin",
		Password:   "wrongpassword",
		AuthMethod: config.AuthMethodPassword,
	}

	_, err := NewClient(cfg)
	if err == nil {
		t.Error("expected error for failed login")
	}
}

func TestClient_ListClusters(t *testing.T) {
	clusters := []Cluster{
		{
			ID:       "c-12345",
			Name:     "dev-cluster",
			State:    "active",
			Provider: "rke",
		},
		{
			ID:       "c-67890",
			Name:     "prod-cluster",
			State:    "active",
			Provider: "eks",
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify authentication header
		user, pass, ok := r.BasicAuth()
		if !ok || user != "access123" || pass != "secret456" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if r.URL.Path == "/v3/clusters" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ClusterCollection{Data: clusters})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cfg := &config.Config{
		RancherURL: server.URL,
		AccessKey:  "access123",
		SecretKey:  "secret456",
		AuthMethod: config.AuthMethodToken,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	result, err := client.ListClusters()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result) != 2 {
		t.Errorf("expected 2 clusters, got %d", len(result))
	}
	if result[0].Name != "dev-cluster" {
		t.Errorf("first cluster name = %q, want %q", result[0].Name, "dev-cluster")
	}
	if result[1].Name != "prod-cluster" {
		t.Errorf("second cluster name = %q, want %q", result[1].Name, "prod-cluster")
	}
}

func TestClient_ListClusters_Unauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message": "unauthorized"}`))
	}))
	defer server.Close()

	cfg := &config.Config{
		RancherURL: server.URL,
		AccessKey:  "wrong",
		SecretKey:  "credentials",
		AuthMethod: config.AuthMethodToken,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	_, err = client.ListClusters()
	if err == nil {
		t.Error("expected error for unauthorized request")
	}
}

func TestClient_GetClusterKubeconfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify bearer token auth
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-bearer-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if r.URL.Path == "/v3/clusters/c-12345" && r.URL.Query().Get("action") == "generateKubeconfig" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(KubeconfigResponse{Config: testKubeconfig})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cfg := &config.Config{
		RancherURL: server.URL,
		AuthMethod: config.AuthMethodToken,
	}

	client := &Client{
		config:      cfg,
		httpClient:  server.Client(),
		bearerToken: "test-bearer-token",
	}

	cluster := &Cluster{
		ID:   "c-12345",
		Name: "test-cluster",
	}

	kubeconfig, err := client.GetClusterKubeconfig(cluster)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if kubeconfig != testKubeconfig {
		t.Error("kubeconfig does not match expected value")
	}
}

func TestClient_GetClusterKubeconfig_WithActionURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/clusters/c-12345/generateKubeconfig" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(KubeconfigResponse{Config: testKubeconfig})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cfg := &config.Config{
		RancherURL: server.URL,
		AccessKey:  "access123",
		SecretKey:  "secret456",
		AuthMethod: config.AuthMethodToken,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	cluster := &Cluster{
		ID:   "c-12345",
		Name: "test-cluster",
		Actions: struct {
			GenerateKubeconfig string `json:"generateKubeconfig"`
		}{
			GenerateKubeconfig: server.URL + "/v3/clusters/c-12345/generateKubeconfig",
		},
	}

	kubeconfig, err := client.GetClusterKubeconfig(cluster)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if kubeconfig != testKubeconfig {
		t.Error("kubeconfig does not match expected value")
	}
}

func TestClient_GetAllKubeconfigs(t *testing.T) {
	clusters := []Cluster{
		{
			ID:    "c-12345",
			Name:  "active-cluster",
			State: "active",
		},
		{
			ID:    "c-67890",
			Name:  "inactive-cluster",
			State: "provisioning",
		},
		{
			ID:    "c-abcde",
			Name:  "another-active",
			State: "active",
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/clusters" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ClusterCollection{Data: clusters})
			return
		}

		if r.Method == "POST" && r.URL.Query().Get("action") == "generateKubeconfig" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(KubeconfigResponse{Config: testKubeconfig})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cfg := &config.Config{
		RancherURL: server.URL,
		AccessKey:  "access123",
		SecretKey:  "secret456",
		AuthMethod: config.AuthMethodToken,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	kubeconfigs, err := client.GetAllKubeconfigs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should only get kubeconfigs for active clusters
	if len(kubeconfigs) != 2 {
		t.Errorf("expected 2 kubeconfigs (active clusters only), got %d", len(kubeconfigs))
	}

	if _, exists := kubeconfigs["active-cluster"]; !exists {
		t.Error("expected kubeconfig for 'active-cluster'")
	}
	if _, exists := kubeconfigs["another-active"]; !exists {
		t.Error("expected kubeconfig for 'another-active'")
	}
	if _, exists := kubeconfigs["inactive-cluster"]; exists {
		t.Error("should not have kubeconfig for inactive cluster")
	}
}

func TestClient_GetAllKubeconfigs_NoClusters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v3/clusters" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ClusterCollection{Data: []Cluster{}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cfg := &config.Config{
		RancherURL: server.URL,
		AccessKey:  "access123",
		SecretKey:  "secret456",
		AuthMethod: config.AuthMethodToken,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	kubeconfigs, err := client.GetAllKubeconfigs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(kubeconfigs) != 0 {
		t.Errorf("expected 0 kubeconfigs, got %d", len(kubeconfigs))
	}
}

func TestClient_BearerTokenAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer my-bearer-token" {
			t.Errorf("expected Bearer auth, got %q", auth)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if r.URL.Path == "/v3/clusters" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ClusterCollection{Data: []Cluster{}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cfg := &config.Config{
		RancherURL: server.URL,
		AuthMethod: config.AuthMethodPassword,
	}

	// Manually set bearer token to simulate successful login
	client := &Client{
		config:      cfg,
		httpClient:  server.Client(),
		bearerToken: "my-bearer-token",
	}

	_, err := client.ListClusters()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoginRequest_JSON(t *testing.T) {
	req := LoginRequest{
		Username:     "admin",
		Password:     "secret",
		ResponseType: "token",
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var decoded LoginRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if decoded.Username != req.Username {
		t.Errorf("Username = %q, want %q", decoded.Username, req.Username)
	}
	if decoded.Password != req.Password {
		t.Errorf("Password = %q, want %q", decoded.Password, req.Password)
	}
	if decoded.ResponseType != req.ResponseType {
		t.Errorf("ResponseType = %q, want %q", decoded.ResponseType, req.ResponseType)
	}
}

func TestCluster_JSON(t *testing.T) {
	jsonData := `{
		"id": "c-12345",
		"name": "test-cluster",
		"description": "A test cluster",
		"state": "active",
		"provider": "rke",
		"links": {
			"self": "https://rancher/v3/clusters/c-12345"
		},
		"actions": {
			"generateKubeconfig": "https://rancher/v3/clusters/c-12345?action=generateKubeconfig"
		}
	}`

	var cluster Cluster
	if err := json.Unmarshal([]byte(jsonData), &cluster); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if cluster.ID != "c-12345" {
		t.Errorf("ID = %q, want %q", cluster.ID, "c-12345")
	}
	if cluster.Name != "test-cluster" {
		t.Errorf("Name = %q, want %q", cluster.Name, "test-cluster")
	}
	if cluster.State != "active" {
		t.Errorf("State = %q, want %q", cluster.State, "active")
	}
	if cluster.Provider != "rke" {
		t.Errorf("Provider = %q, want %q", cluster.Provider, "rke")
	}
	if cluster.Actions.GenerateKubeconfig == "" {
		t.Error("Actions.GenerateKubeconfig should not be empty")
	}
}
