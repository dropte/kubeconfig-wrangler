package provider

import (
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

const staticKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c1
  cluster:
    server: https://c1.example.com
- name: c2
  cluster:
    server: https://c2.example.com
users:
- name: u1
  user:
    token: t1
contexts:
- name: gamma
  context: {cluster: c1, user: u1}
- name: alpha
  context: {cluster: c2, user: u1}
- name: beta
  context: {cluster: c1, user: u1}
- name: missing-user
  context: {cluster: c1, user: nobody}
- name: missing-cluster
  context: {cluster: nowhere, user: u1}
- name: no-user
  context: {cluster: c1}
current-context: gamma
`

func TestStaticProvider_ListClustersSortedAndComplete(t *testing.T) {
	for i := 0; i < 20; i++ {
		p, err := NewStaticProvider(StaticConfig{ProfileID: "p", ProfileName: "P", Kubeconfig: staticKubeconfig})
		if err != nil {
			t.Fatal(err)
		}
		clusters, _ := p.ListClusters()
		var names []string
		for _, c := range clusters {
			names = append(names, c.Name)
		}
		if got := strings.Join(names, ","); got != "alpha,beta,gamma" {
			t.Fatalf("clusters = %s, want alpha,beta,gamma", got)
		}
	}
}

func TestStaticProvider_GetKubeconfig(t *testing.T) {
	p, err := NewStaticProvider(StaticConfig{Kubeconfig: staticKubeconfig})
	if err != nil {
		t.Fatal(err)
	}

	out, err := p.GetKubeconfig("alpha")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.Load([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if err := clientcmd.Validate(*cfg); err != nil {
		t.Errorf("exported kubeconfig is invalid: %v", err)
	}
	if len(cfg.Clusters) != 1 || cfg.Clusters["c2"] == nil || len(cfg.AuthInfos) != 1 || cfg.AuthInfos["u1"] == nil {
		t.Errorf("unexpected content: clusters=%v users=%v", cfg.Clusters, cfg.AuthInfos)
	}

	for _, name := range []string{"missing-user", "missing-cluster", "no-user", "does-not-exist"} {
		if _, err := p.GetKubeconfig(name); err == nil {
			t.Errorf("GetKubeconfig(%q) succeeded, want error", name)
		}
	}
}

const relativePathKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: rel
  cluster:
    server: https://rel.example.com
    certificate-authority: certs/ca.pem
- name: abs
  cluster:
    server: https://abs.example.com
    certificate-authority: /etc/k8s/ca.pem
users:
- name: rel-user
  user:
    client-certificate: ./client.crt
    client-key: client.key
    tokenFile: token.txt
- name: exec-rel
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: bin/get-token
- name: exec-bare
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws
contexts:
- name: relative
  context: {cluster: rel, user: rel-user}
- name: rel-exec
  context: {cluster: abs, user: exec-rel}
- name: fine
  context: {cluster: abs, user: exec-bare}
`

func TestStaticProvider_RelativePathsRejectedOnExport(t *testing.T) {
	p, err := NewStaticProvider(StaticConfig{Kubeconfig: relativePathKubeconfig})
	if err != nil {
		t.Fatal(err)
	}

	_, err = p.GetKubeconfig("relative")
	if err == nil {
		t.Fatal("expected relative paths to be rejected")
	}
	for _, want := range []string{"certs/ca.pem", "./client.crt", "client.key", "token.txt", "absolute paths"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if _, err := p.GetKubeconfig("rel-exec"); err == nil || !strings.Contains(err.Error(), "bin/get-token") {
		t.Errorf("rel-exec: error = %v, want relative exec command rejected", err)
	}
	if _, err := p.GetKubeconfig("fine"); err != nil {
		t.Errorf("absolute CA and bare exec command must export: %v", err)
	}

	clusters, _ := p.ListClusters()
	for _, c := range clusters {
		warned := strings.Contains(c.Description, "relative file paths")
		if warned != (c.Name != "fine") {
			t.Errorf("cluster %s description = %q", c.Name, c.Description)
		}
	}
}

func TestRelativePathProblems(t *testing.T) {
	problems, err := RelativePathProblems(relativePathKubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 5 {
		t.Errorf("problems = %v, want 5", problems)
	}
	if got, _ := RelativePathProblems(staticKubeconfig); len(got) != 0 {
		t.Errorf("staticKubeconfig problems = %v, want none", got)
	}
}
