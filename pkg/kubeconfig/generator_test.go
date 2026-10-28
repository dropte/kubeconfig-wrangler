package kubeconfig

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"

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

		// With no contexts there is nothing to point current-context at; the
		// old behaviour left a dangling "test-my-cluster" here.
		if prefixedConfig.CurrentContext != "" {
			t.Errorf("current-context should be empty without contexts, got %q", prefixedConfig.CurrentContext)
		}
		// The unreferenced cluster is kept under a derived name.
		if _, ok := prefixedConfig.Clusters["test-my-cluster-my-cluster"]; !ok {
			t.Errorf("unreferenced cluster not kept, clusters = %v", prefixedConfig.Clusters)
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

// aceKubeconfig mimics Rancher's generateKubeconfig output for a cluster with
// the authorized cluster endpoint: the Rancher-proxied context (current) plus
// one direct context per control-plane node, all sharing one user.
const aceKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: prod
  cluster:
    server: https://rancher.example.com/k8s/clusters/c-abc
- name: prod-cp1
  cluster:
    server: https://10.0.0.1:6443
- name: prod-cp2
  cluster:
    server: https://10.0.0.2:6443
users:
- name: prod
  user:
    token: kubeconfig-u-abc:secret
contexts:
- name: prod
  context:
    cluster: prod
    user: prod
- name: prod-cp1
  context:
    cluster: prod-cp1
    user: prod
- name: prod-cp2
  context:
    cluster: prod-cp2
    user: prod
current-context: prod
`

func serverOf(t *testing.T, cfg *api.Config, ctxName string) string {
	t.Helper()
	ctx, ok := cfg.Contexts[ctxName]
	if !ok {
		t.Fatalf("context %q missing; have %v", ctxName, sortedKeys(cfg.Contexts))
	}
	cl, ok := cfg.Clusters[ctx.Cluster]
	if !ok {
		t.Fatalf("context %q references missing cluster %q", ctxName, ctx.Cluster)
	}
	if _, ok := cfg.AuthInfos[ctx.AuthInfo]; !ok {
		t.Fatalf("context %q references missing user %q", ctxName, ctx.AuthInfo)
	}
	return cl.Server
}

func TestGenerate_ACEDeterministicAndNamePreserving(t *testing.T) {
	g := NewGenerator("x-")
	in := map[string]string{"prod": aceKubeconfig}

	first, err := g.Generate(in)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		out, err := g.Generate(in)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, first) {
			t.Fatalf("run %d produced different output:\n%s\nvs\n%s", i, out, first)
		}
	}

	cfg, err := clientcmd.Load(first)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"x-prod":     "https://rancher.example.com/k8s/clusters/c-abc",
		"x-prod-cp1": "https://10.0.0.1:6443",
		"x-prod-cp2": "https://10.0.0.2:6443",
	}
	if len(cfg.Contexts) != len(want) {
		t.Fatalf("contexts = %v, want %v", sortedKeys(cfg.Contexts), want)
	}
	for name, server := range want {
		if got := serverOf(t, cfg, name); got != server {
			t.Errorf("context %s -> %s, want %s", name, got, server)
		}
		if cfg.Contexts[name].AuthInfo != "x-prod" {
			t.Errorf("context %s user = %q, want x-prod", name, cfg.Contexts[name].AuthInfo)
		}
	}
	if cfg.CurrentContext != "x-prod" {
		t.Errorf("current-context = %q, want x-prod", cfg.CurrentContext)
	}
}

func TestApplyPrefix_BaseContextWithoutCurrentContext(t *testing.T) {
	g := NewGenerator("")
	cfg := &api.Config{
		Clusters: map[string]*api.Cluster{
			"b-cluster": {Server: "https://b"},
			"a-cluster": {Server: "https://a"},
		},
		AuthInfos: map[string]*api.AuthInfo{"u": {Token: "t"}},
		Contexts: map[string]*api.Context{
			"zeta":  {Cluster: "b-cluster", AuthInfo: "u"},
			"alpha": {Cluster: "a-cluster", AuthInfo: "u"},
		},
		CurrentContext: "does-not-exist",
	}
	for i := 0; i < 20; i++ {
		out := g.ApplyPrefix(cfg, "c")
		if out.CurrentContext != "c" {
			t.Fatalf("current-context = %q", out.CurrentContext)
		}
		if got := serverOf(t, out, "c"); got != "https://a" {
			t.Fatalf("base context server = %s, want https://a (smallest context name)", got)
		}
		if got := serverOf(t, out, "c-zeta"); got != "https://b" {
			t.Fatalf("c-zeta server = %s, want https://b", got)
		}
	}
}

func TestApplyPrefix_StripsOnlyAtSeparator(t *testing.T) {
	g := NewGenerator("")
	cfg := &api.Config{
		Clusters: map[string]*api.Cluster{"prod": {}, "production": {}},
		Contexts: map[string]*api.Context{
			"prod":       {Cluster: "prod"},
			"production": {Cluster: "production"},
		},
		CurrentContext: "prod",
	}
	out := g.ApplyPrefix(cfg, "k")
	if _, ok := out.Contexts["k-production"]; !ok {
		t.Errorf("contexts = %v, want k-production", sortedKeys(out.Contexts))
	}
}

func TestMergeConfigs_CollisionsNeverOverwrite(t *testing.T) {
	// Source "prod" derives "prod-cp1"; another source is literally named
	// "prod-cp1". Neither may overwrite the other, and every context must
	// keep its own server and credentials.
	single := `apiVersion: v1
kind: Config
clusters:
- name: other
  cluster:
    server: https://other.example.com
users:
- name: other
  user:
    token: other-token
contexts:
- name: other
  context:
    cluster: other
    user: other
current-context: other
`
	g := NewGenerator("")
	in := map[string]string{"prod": aceKubeconfig, "prod-cp1": single}

	first, names, err := g.GenerateWithNames(in)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		out, n2, err := g.GenerateWithNames(in)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, first) || !reflect.DeepEqual(n2, names) {
			t.Fatal("output not deterministic")
		}
	}
	cfg, err := clientcmd.Load(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Contexts) != 4 || len(cfg.Clusters) != 4 || len(cfg.AuthInfos) != 2 {
		t.Fatalf("lost entries: contexts=%v clusters=%v users=%v",
			sortedKeys(cfg.Contexts), sortedKeys(cfg.Clusters), sortedKeys(cfg.AuthInfos))
	}
	// The explicitly selected source keeps its exact base name.
	if got := serverOf(t, cfg, "prod-cp1"); got != "https://other.example.com" {
		t.Errorf("prod-cp1 -> %s, want other cluster", got)
	}
	if tok := cfg.AuthInfos[cfg.Contexts["prod-cp1"].AuthInfo].Token; tok != "other-token" {
		t.Errorf("prod-cp1 uses token %q", tok)
	}
	// The derived node context is suffixed instead.
	if got := serverOf(t, cfg, "prod-cp1-2"); got != "https://10.0.0.1:6443" {
		t.Errorf("prod-cp1-2 -> %s", got)
	}
	if tok := cfg.AuthInfos[cfg.Contexts["prod-cp1-2"].AuthInfo].Token; tok != "kubeconfig-u-abc:secret" {
		t.Errorf("prod-cp1-2 uses token %q", tok)
	}
	if got := serverOf(t, cfg, "prod"); got != "https://rancher.example.com/k8s/clusters/c-abc" {
		t.Errorf("prod -> %s", got)
	}

	wantNames := map[string][]string{
		"prod":     {"prod", "prod-cp1-2", "prod-cp2"},
		"prod-cp1": {"prod-cp1"},
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("names = %v, want %v", names, wantNames)
	}
	// F039: current-context is the base context of the first sorted key.
	if cfg.CurrentContext != "prod" {
		t.Errorf("current-context = %q, want prod", cfg.CurrentContext)
	}
}

func TestMergeConfigs_SameDerivedNamesAcrossSources(t *testing.T) {
	// Two sources whose non-base entries derive to the same final name.
	mk := func(server string) string {
		return `apiVersion: v1
kind: Config
clusters:
- name: a
  cluster:
    server: ` + server + `/a
- name: b
  cluster:
    server: ` + server + `/b
users:
- name: u
  user:
    token: ` + server + `
contexts:
- name: a
  context: {cluster: a, user: u}
- name: b
  context: {cluster: b, user: u}
current-context: a
`
	}
	g := NewGenerator("")
	// Source "x" derives "x-b"; source "x-b" has base "x-b" and derives "x-b-b".
	cfg, err := g.MergeConfigs(map[string]string{"x": mk("https://one"), "x-b": mk("https://two")})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"x":     "https://one/a",
		"x-b-2": "https://one/b",
		"x-b":   "https://two/a",
		"x-b-b": "https://two/b",
	}
	if len(cfg.Contexts) != len(want) {
		t.Fatalf("contexts = %v", sortedKeys(cfg.Contexts))
	}
	for name, server := range want {
		if got := serverOf(t, cfg, name); got != server {
			t.Errorf("%s -> %s, want %s", name, got, server)
		}
	}
	if cfg.AuthInfos[cfg.Contexts["x-b-2"].AuthInfo].Token != "https://one" {
		t.Error("x-b-2 wired to wrong credentials")
	}
}

func TestGenerate_SingleClusterHasCurrentContext(t *testing.T) {
	g := NewGenerator("")
	data, err := g.Generate(map[string]string{"my-cluster": sampleKubeconfig})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext != "my-cluster" {
		t.Errorf("current-context = %q, want my-cluster", cfg.CurrentContext)
	}
	// Loadable the way kubectl does, without --context.
	if _, err := clientcmd.NewDefaultClientConfig(*cfg, nil).ClientConfig(); err != nil {
		t.Errorf("config not usable without --context: %v", err)
	}
}

func TestGenerate_TagsAreEscaped(t *testing.T) {
	tags := []string{`say "hi"`, `c:\path`, "line1\nline2", "plain"}
	g := NewGenerator("")
	g.SetAllTags(tags)
	data, err := g.Generate(map[string]string{"my-cluster": sampleKubeconfig})
	if err != nil {
		t.Fatalf("generate failed: %v", err)
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		t.Fatalf("output does not parse: %v\n%s", err, data)
	}
	ext, ok := cfg.Contexts["my-cluster"].Extensions["aptakube"]
	if !ok {
		t.Fatalf("aptakube extension missing:\n%s", data)
	}
	unk, ok := ext.(*runtime.Unknown)
	if !ok {
		t.Fatalf("extension type %T", ext)
	}
	var got AptakubeExtension
	if err := json.Unmarshal(unk.Raw, &got); err != nil {
		t.Fatalf("extension JSON invalid: %v (%s)", err, unk.Raw)
	}
	if !reflect.DeepEqual(got.Tags, tags) {
		t.Errorf("tags = %q, want %q", got.Tags, tags)
	}
}
