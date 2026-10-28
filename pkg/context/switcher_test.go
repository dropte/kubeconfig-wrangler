package context

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

const existingConfig = `apiVersion: v1
kind: Config
current-context: mine
clusters:
- name: mine
  cluster: {server: "https://mine:6443"}
- name: prod
  cluster: {server: "https://handmade-prod:6443"}
users:
- name: mine
  user: {token: m}
- name: prod
  user: {token: p}
contexts:
- name: mine
  context: {cluster: mine, user: mine}
- name: prod
  context: {cluster: prod, user: prod, namespace: team-a}
`

const generatedConfig = `apiVersion: v1
kind: Config
current-context: prod
clusters:
- name: prod
  cluster: {server: "https://rancher/k8s/clusters/c-1"}
- name: dev
  cluster: {server: "https://rancher/k8s/clusters/c-2"}
users:
- name: prod
  user: {token: new}
- name: dev
  user: {token: new}
contexts:
- name: prod
  context: {cluster: prod, user: prod}
- name: dev
  context: {cluster: dev, user: dev}
`

func writeKubeconfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestMergeKubeconfigReport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	writeKubeconfig(t, path, existingConfig)

	s := NewSwitcherWithPath(path)
	added, replaced, err := s.MergeKubeconfigReport(generatedConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(added, []string{"dev"}) || !reflect.DeepEqual(replaced, []string{"prod"}) {
		t.Fatalf("added=%v replaced=%v", added, replaced)
	}

	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext != "mine" {
		t.Errorf("current-context = %q, want mine", cfg.CurrentContext)
	}
	if cfg.Contexts["prod"].Namespace != "team-a" {
		t.Errorf("namespace not preserved: %+v", cfg.Contexts["prod"])
	}
	if cfg.Clusters["prod"].Server != "https://rancher/k8s/clusters/c-1" {
		t.Errorf("cluster not merged: %+v", cfg.Clusters["prod"])
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestMergeKubeconfigAdoptsCurrentContextWhenEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	s := NewSwitcherWithPath(path)
	if err := s.MergeKubeconfig(generatedConfig); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.GetCurrentContext(); cur != "prod" {
		t.Errorf("current-context = %q, want prod", cur)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Error("no backup expected for a new file")
	}
}

func TestSaveConfigAtomicWithSingleBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	writeKubeconfig(t, path, existingConfig)

	s := NewSwitcherWithPath(path)
	if err := s.SetCurrentContext("prod"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCurrentContext("mine"); err != nil {
		t.Fatal(err)
	}

	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(bak) != existingConfig {
		t.Error(".bak should hold the original file, written once")
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
	if len(entries) != 2 {
		t.Errorf("unexpected files: %v", entries)
	}
}

func TestSaveConfigKeepsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-config")
	link := filepath.Join(dir, "config")
	writeKubeconfig(t, real, existingConfig)
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unsupported:", err)
	}

	s := NewSwitcherWithPath(link)
	if err := s.SetCurrentContext("prod"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced by a regular file")
	}
	cfg, err := clientcmd.LoadFromFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext != "prod" {
		t.Errorf("target not updated: %q", cfg.CurrentContext)
	}
}
