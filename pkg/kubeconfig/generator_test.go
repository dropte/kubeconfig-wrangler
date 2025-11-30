package kubeconfig

import (
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd/api"
)

// Sample kubeconfig for testing
const sampleKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://cluster1.example.com:6443
    certificate-authority-data: dGVzdC1jYS1kYXRh
  name: my-cluster
contexts:
- context:
    cluster: my-cluster
    user: my-user
  name: my-cluster
current-context: my-cluster
users:
- name: my-user
  user:
    token: test-token-12345
`

const sampleKubeconfig2 = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://cluster2.example.com:6443
    certificate-authority-data: dGVzdC1jYS1kYXRhMg==
  name: another-cluster
contexts:
- context:
    cluster: another-cluster
    user: another-user
  name: another-cluster
current-context: another-cluster
users:
- name: another-user
  user:
    token: test-token-67890
`

func TestNewGenerator(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
	}{
		{"empty prefix", ""},
		{"with prefix", "prod-"},
		{"with trailing dash", "staging-"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGenerator(tt.prefix)
			if g == nil {
				t.Fatal("NewGenerator returned nil")
			}
			if g.prefix != tt.prefix {
				t.Errorf("prefix = %q, want %q", g.prefix, tt.prefix)
			}
		})
	}
}

func TestGenerator_ParseKubeconfig(t *testing.T) {
	g := NewGenerator("")

	t.Run("valid kubeconfig", func(t *testing.T) {
		config, err := g.ParseKubeconfig(sampleKubeconfig)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(config.Clusters) != 1 {
			t.Errorf("expected 1 cluster, got %d", len(config.Clusters))
		}
		if _, exists := config.Clusters["my-cluster"]; !exists {
			t.Error("expected cluster 'my-cluster' to exist")
		}
		if len(config.Contexts) != 1 {
			t.Errorf("expected 1 context, got %d", len(config.Contexts))
		}
		if len(config.AuthInfos) != 1 {
			t.Errorf("expected 1 user, got %d", len(config.AuthInfos))
		}
		if config.CurrentContext != "my-cluster" {
			t.Errorf("current-context = %q, want %q", config.CurrentContext, "my-cluster")
		}
	})

	t.Run("invalid kubeconfig", func(t *testing.T) {
		_, err := g.ParseKubeconfig("this is not valid yaml: {{{}}")
		if err == nil {
			t.Error("expected error for invalid kubeconfig")
		}
	})

	t.Run("empty kubeconfig", func(t *testing.T) {
		config, err := g.ParseKubeconfig("")
		if err != nil {
			t.Fatalf("unexpected error for empty config: %v", err)
		}
		// Empty kubeconfig should parse but have no entries
		if len(config.Clusters) != 0 {
			t.Errorf("expected 0 clusters for empty config, got %d", len(config.Clusters))
		}
	})
}

func TestGenerator_ApplyPrefix(t *testing.T) {
	t.Run("with prefix", func(t *testing.T) {
		g := NewGenerator("prod-")
		config := &api.Config{
			Clusters: map[string]*api.Cluster{
				"my-cluster": {Server: "https://server.example.com"},
			},
			Contexts: map[string]*api.Context{
				"my-cluster": {
					Cluster:  "my-cluster",
					AuthInfo: "my-user",
				},
			},
			AuthInfos: map[string]*api.AuthInfo{
				"my-user": {Token: "test-token"},
			},
			CurrentContext: "my-cluster",
		}

		prefixedConfig := g.ApplyPrefix(config, "my-cluster")

		// Check clusters - renamed to use clusterName as base
		if _, exists := prefixedConfig.Clusters["prod-my-cluster"]; !exists {
			t.Error("expected prefixed cluster 'prod-my-cluster' to exist")
		}
		if _, exists := prefixedConfig.Clusters["my-cluster"]; exists {
			t.Error("original cluster name should not exist after prefixing")
		}

		// Check contexts - renamed to use clusterName as base
		if _, exists := prefixedConfig.Contexts["prod-my-cluster"]; !exists {
			t.Error("expected prefixed context 'prod-my-cluster' to exist")
		}
		if prefixedConfig.Contexts["prod-my-cluster"].Cluster != "prod-my-cluster" {
			t.Errorf("context cluster reference = %q, want %q",
				prefixedConfig.Contexts["prod-my-cluster"].Cluster, "prod-my-cluster")
		}
		// Auth info is also renamed to use clusterName as base (not original user name)
		if prefixedConfig.Contexts["prod-my-cluster"].AuthInfo != "prod-my-cluster" {
			t.Errorf("context auth info reference = %q, want %q",
				prefixedConfig.Contexts["prod-my-cluster"].AuthInfo, "prod-my-cluster")
		}

		// Check auth infos - renamed to use clusterName as base
		if _, exists := prefixedConfig.AuthInfos["prod-my-cluster"]; !exists {
			t.Error("expected prefixed user 'prod-my-cluster' to exist")
		}

		// Check current context
		if prefixedConfig.CurrentContext != "prod-my-cluster" {
			t.Errorf("current-context = %q, want %q", prefixedConfig.CurrentContext, "prod-my-cluster")
		}
	})

	t.Run("without prefix", func(t *testing.T) {
		g := NewGenerator("")
		config := &api.Config{
			Clusters: map[string]*api.Cluster{
				"my-cluster": {Server: "https://server.example.com"},
			},
			Contexts: map[string]*api.Context{
				"my-cluster": {
					Cluster:  "my-cluster",
					AuthInfo: "my-user",
				},
			},
			AuthInfos: map[string]*api.AuthInfo{
				"my-user": {Token: "test-token"},
			},
			CurrentContext: "my-cluster",
		}

		prefixedConfig := g.ApplyPrefix(config, "my-cluster")

		// Without prefix, names are still renamed to use clusterName as base
		if _, exists := prefixedConfig.Clusters["my-cluster"]; !exists {
			t.Error("cluster name should be 'my-cluster' (clusterName as base)")
		}
		if _, exists := prefixedConfig.Contexts["my-cluster"]; !exists {
			t.Error("context name should be 'my-cluster' (clusterName as base)")
		}
		// User name is also renamed to use clusterName as base
		if _, exists := prefixedConfig.AuthInfos["my-cluster"]; !exists {
			t.Error("user name should be 'my-cluster' (clusterName as base)")
		}
		if prefixedConfig.CurrentContext != "my-cluster" {
			t.Errorf("current-context should be 'my-cluster', got %q", prefixedConfig.CurrentContext)
		}
	})

	t.Run("empty current context", func(t *testing.T) {
		g := NewGenerator("test-")
		config := &api.Config{
			Clusters: map[string]*api.Cluster{
				"my-cluster": {Server: "https://server.example.com"},
			},
			CurrentContext: "",
		}

		prefixedConfig := g.ApplyPrefix(config, "my-cluster")

		// ApplyPrefix always sets current context to the new name
		if prefixedConfig.CurrentContext != "test-my-cluster" {
			t.Errorf("current-context should be 'test-my-cluster', got %q", prefixedConfig.CurrentContext)
		}
	})
}

func TestGenerator_MergeConfigs(t *testing.T) {
	t.Run("merge multiple configs without prefix", func(t *testing.T) {
		g := NewGenerator("")

		kubeconfigs := map[string]string{
			"my-cluster":      sampleKubeconfig,
			"another-cluster": sampleKubeconfig2,
		}

		merged, err := g.MergeConfigs(kubeconfigs)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Should have 2 clusters
		if len(merged.Clusters) != 2 {
			t.Errorf("expected 2 clusters, got %d", len(merged.Clusters))
		}
		if _, exists := merged.Clusters["my-cluster"]; !exists {
			t.Error("expected 'my-cluster' to exist")
		}
		if _, exists := merged.Clusters["another-cluster"]; !exists {
			t.Error("expected 'another-cluster' to exist")
		}

		// Should have 2 contexts
		if len(merged.Contexts) != 2 {
			t.Errorf("expected 2 contexts, got %d", len(merged.Contexts))
		}

		// Should have 2 users
		if len(merged.AuthInfos) != 2 {
			t.Errorf("expected 2 users, got %d", len(merged.AuthInfos))
		}
	})

	t.Run("merge multiple configs with prefix", func(t *testing.T) {
		g := NewGenerator("dev-")

		kubeconfigs := map[string]string{
			"my-cluster":      sampleKubeconfig,
			"another-cluster": sampleKubeconfig2,
		}

		merged, err := g.MergeConfigs(kubeconfigs)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Should have 2 clusters with prefix
		if len(merged.Clusters) != 2 {
			t.Errorf("expected 2 clusters, got %d", len(merged.Clusters))
		}
		if _, exists := merged.Clusters["dev-my-cluster"]; !exists {
			t.Error("expected 'dev-my-cluster' to exist")
		}
		if _, exists := merged.Clusters["dev-another-cluster"]; !exists {
			t.Error("expected 'dev-another-cluster' to exist")
		}

		// Check that unprefixed names don't exist
		if _, exists := merged.Clusters["my-cluster"]; exists {
			t.Error("unprefixed 'my-cluster' should not exist")
		}
	})

	t.Run("invalid kubeconfig in merge", func(t *testing.T) {
		g := NewGenerator("")

		kubeconfigs := map[string]string{
			"valid":   sampleKubeconfig,
			"invalid": "not valid yaml: {{{}",
		}

		_, err := g.MergeConfigs(kubeconfigs)
		if err == nil {
			t.Error("expected error when merging invalid kubeconfig")
		}
	})

	t.Run("empty configs map", func(t *testing.T) {
		g := NewGenerator("")

		merged, err := g.MergeConfigs(map[string]string{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(merged.Clusters) != 0 {
			t.Errorf("expected 0 clusters, got %d", len(merged.Clusters))
		}
	})
}

func TestGenerator_Serialize(t *testing.T) {
	g := NewGenerator("")

	config := &api.Config{
		Clusters: map[string]*api.Cluster{
			"test-cluster": {Server: "https://server.example.com:6443"},
		},
		Contexts: map[string]*api.Context{
			"test-cluster": {
				Cluster:  "test-cluster",
				AuthInfo: "test-user",
			},
		},
		AuthInfos: map[string]*api.AuthInfo{
			"test-user": {Token: "test-token"},
		},
		CurrentContext: "test-cluster",
	}

	data, err := g.Serialize(config)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check that the output is valid YAML containing expected elements
	output := string(data)

	if !strings.Contains(output, "apiVersion:") {
		t.Error("serialized config should contain apiVersion")
	}
	if !strings.Contains(output, "kind: Config") {
		t.Error("serialized config should contain 'kind: Config'")
	}
	if !strings.Contains(output, "test-cluster") {
		t.Error("serialized config should contain cluster name")
	}
	if !strings.Contains(output, "https://server.example.com:6443") {
		t.Error("serialized config should contain server URL")
	}

	// Verify it can be parsed back
	parsed, err := g.ParseKubeconfig(output)
	if err != nil {
		t.Fatalf("failed to parse serialized config: %v", err)
	}

	if len(parsed.Clusters) != 1 {
		t.Errorf("parsed config should have 1 cluster, got %d", len(parsed.Clusters))
	}
}

func TestGenerator_Generate(t *testing.T) {
	t.Run("full generation flow", func(t *testing.T) {
		g := NewGenerator("prod-")

		kubeconfigs := map[string]string{
			"my-cluster":      sampleKubeconfig,
			"another-cluster": sampleKubeconfig2,
		}

		data, err := g.Generate(kubeconfigs)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		output := string(data)

		// Check that prefixed names appear in output
		if !strings.Contains(output, "prod-my-cluster") {
			t.Error("output should contain 'prod-my-cluster'")
		}
		if !strings.Contains(output, "prod-another-cluster") {
			t.Error("output should contain 'prod-another-cluster'")
		}

		// Verify the output can be parsed
		parsed, err := g.ParseKubeconfig(output)
		if err != nil {
			t.Fatalf("failed to parse generated config: %v", err)
		}

		if len(parsed.Clusters) != 2 {
			t.Errorf("expected 2 clusters in generated config, got %d", len(parsed.Clusters))
		}
	})

	t.Run("generate with invalid input", func(t *testing.T) {
		g := NewGenerator("")

		kubeconfigs := map[string]string{
			"invalid": "{{{{invalid yaml",
		}

		_, err := g.Generate(kubeconfigs)
		if err == nil {
			t.Error("expected error for invalid input")
		}
	})

	t.Run("generate single cluster", func(t *testing.T) {
		g := NewGenerator("")

		kubeconfigs := map[string]string{
			"my-cluster": sampleKubeconfig,
		}

		data, err := g.Generate(kubeconfigs)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		parsed, err := g.ParseKubeconfig(string(data))
		if err != nil {
			t.Fatalf("failed to parse: %v", err)
		}

		if len(parsed.Clusters) != 1 {
			t.Errorf("expected 1 cluster, got %d", len(parsed.Clusters))
		}
	})
}
