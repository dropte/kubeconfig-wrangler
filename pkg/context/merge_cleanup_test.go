package context

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

// R011: clusters and users replaced with different content are reported.
func TestMergeKubeconfigResultReportsClustersAndUsers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeKubeconfig(t, path, existingConfig)

	s := NewSwitcherWithPath(path)
	res, err := s.MergeKubeconfigResult(generatedConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.ReplacedClusters, []string{"prod"}) || !reflect.DeepEqual(res.ReplacedUsers, []string{"prod"}) {
		t.Fatalf("replaced clusters=%v users=%v", res.ReplacedClusters, res.ReplacedUsers)
	}
	if res.RenamedClusters != nil || res.RenamedUsers != nil {
		t.Fatalf("unexpected renames: %+v", res)
	}

	// A routine refresh with identical content reports no replacements.
	res, err = s.MergeKubeconfigResult(generatedConfig)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ReplacedClusters) != 0 || len(res.ReplacedUsers) != 0 {
		t.Fatalf("identical refresh reported replacements: %+v", res)
	}
	if !reflect.DeepEqual(res.Replaced, []string{"dev", "prod"}) {
		t.Fatalf("replaced contexts = %v", res.Replaced)
	}
}

const sharedExisting = `apiVersion: v1
kind: Config
current-context: legacy
clusters:
- name: prod
  cluster: {server: "https://handmade-prod:6443"}
users:
- name: prod
  user: {token: p}
contexts:
- name: legacy
  context: {cluster: prod, user: prod}
`

// R011: an existing cluster/user still used by a context outside the merge
// is not overwritten; the incoming entry gets a unique name.
func TestMergeKubeconfigResultKeepsEntriesUsedElsewhere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	writeKubeconfig(t, path, sharedExisting)

	s := NewSwitcherWithPath(path)
	res, err := s.MergeKubeconfigResult(generatedConfig)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"prod": "prod-2"}
	if !reflect.DeepEqual(res.RenamedClusters, want) || !reflect.DeepEqual(res.RenamedUsers, want) {
		t.Fatalf("renames: clusters=%v users=%v", res.RenamedClusters, res.RenamedUsers)
	}

	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Clusters["prod"].Server != "https://handmade-prod:6443" || cfg.AuthInfos["prod"].Token != "p" {
		t.Fatal("hand-made entry used by legacy was overwritten")
	}
	if cfg.Clusters["prod-2"].Server != "https://rancher/k8s/clusters/c-1" {
		t.Fatalf("incoming cluster not written under new name: %+v", cfg.Clusters)
	}
	if c := cfg.Contexts["prod"]; c.Cluster != "prod-2" || c.AuthInfo != "prod-2" {
		t.Fatalf("merged context not remapped: %+v", c)
	}
	if c := cfg.Contexts["legacy"]; c.Cluster != "prod" || c.AuthInfo != "prod" {
		t.Fatalf("legacy context repointed: %+v", c)
	}

	// Merging a refreshed config again reuses prod-2 instead of creating prod-3.
	refreshed := `apiVersion: v1
kind: Config
clusters:
- name: prod
  cluster: {server: "https://rancher/k8s/clusters/c-1"}
users:
- name: prod
  user: {token: rotated}
contexts:
- name: prod
  context: {cluster: prod, user: prod}
`
	res, err = s.MergeKubeconfigResult(refreshed)
	if err != nil {
		t.Fatal(err)
	}
	if res.RenamedUsers["prod"] != "prod-2" || !reflect.DeepEqual(res.ReplacedUsers, []string{"prod-2"}) {
		t.Fatalf("second merge: %+v", res)
	}
	cfg, _ = clientcmd.LoadFromFile(path)
	if _, ok := cfg.AuthInfos["prod-3"]; ok {
		t.Error("refresh created another renamed entry")
	}
	if cfg.AuthInfos["prod-2"].Token != "rotated" || cfg.AuthInfos["prod"].Token != "p" {
		t.Errorf("users after refresh: %+v", cfg.AuthInfos)
	}
}

// R012: orphan cleanup considers contexts in every KUBECONFIG file.
func TestDeleteContextCleanupMultiFileKubeconfig(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	writeKubeconfig(t, a, `apiVersion: v1
kind: Config
clusters:
- name: shared
  cluster: {server: "https://shared:6443"}
- name: only-a
  cluster: {server: "https://only-a:6443"}
users:
- name: shared
  user: {token: s}
- name: only-a
  user: {token: a}
contexts:
- name: ctx-a
  context: {cluster: shared, user: shared}
- name: ctx-only-a
  context: {cluster: only-a, user: only-a}
`)
	writeKubeconfig(t, b, `apiVersion: v1
kind: Config
contexts:
- name: ctx-b
  context: {cluster: shared, user: shared}
`)
	missing := filepath.Join(dir, "missing")
	t.Setenv("KUBECONFIG", a+string(os.PathListSeparator)+b+string(os.PathListSeparator)+missing)

	s := NewSwitcher()
	if s.Path() != a {
		t.Fatalf("path = %q", s.Path())
	}
	if err := s.DeleteContextWithCleanup("ctx-a", true); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteContextWithCleanup("ctx-only-a", true); err != nil {
		t.Fatal(err)
	}

	cfg, err := clientcmd.LoadFromFile(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Clusters["shared"]; !ok {
		t.Error("cluster used by a context in file b was deleted")
	}
	if _, ok := cfg.AuthInfos["shared"]; !ok {
		t.Error("user used by a context in file b was deleted")
	}
	if _, ok := cfg.Clusters["only-a"]; ok {
		t.Error("orphaned cluster not cleaned up")
	}
	if _, ok := cfg.AuthInfos["only-a"]; ok {
		t.Error("orphaned user not cleaned up")
	}
	if len(cfg.Contexts) != 0 {
		t.Errorf("contexts = %v", cfg.Contexts)
	}
	if bCfg, _ := clientcmd.LoadFromFile(b); len(bCfg.Contexts) != 1 {
		t.Error("file b was modified")
	}
}

// R022: a rename refused with EBUSY/EXDEV (single-file bind mount) falls
// back to an in-place rewrite that keeps the inode and mode.
func TestSaveConfigFallsBackToInPlaceWrite(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EBUSY, syscall.EXDEV} {
		t.Run(errno.Error(), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config")
			writeKubeconfig(t, path, existingConfig)
			before, _ := os.Stat(path)

			old := renameFile
			renameFile = func(from, to string) error {
				if filepath.Base(to) == filepath.Base(path) {
					return &os.LinkError{Op: "rename", Old: from, New: to, Err: errno}
				}
				return old(from, to)
			}
			t.Cleanup(func() { renameFile = old })

			s := NewSwitcherWithPath(path)
			if err := s.SetCurrentContext("prod"); err != nil {
				t.Fatal(err)
			}
			if cur, _ := s.GetCurrentContext(); cur != "prod" {
				t.Fatalf("current-context = %q", cur)
			}
			after, _ := os.Stat(path)
			if !os.SameFile(before, after) {
				t.Error("in-place write replaced the file")
			}
			if after.Mode().Perm() != 0644 {
				t.Errorf("mode = %v, want 0644 kept", after.Mode().Perm())
			}
			if b, _ := os.ReadFile(path + ".bak"); string(b) != existingConfig {
				t.Error(".bak not written before the in-place write")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 2 {
				t.Errorf("leftover files: %v", entries)
			}
		})
	}

	// Other rename errors are returned.
	path := filepath.Join(t.TempDir(), "config")
	writeKubeconfig(t, path, existingConfig)
	old := renameFile
	renameFile = func(from, to string) error {
		if filepath.Base(to) == filepath.Base(path) {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: syscall.EACCES}
		}
		return old(from, to)
	}
	t.Cleanup(func() { renameFile = old })
	if err := NewSwitcherWithPath(path).SetCurrentContext("prod"); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("err = %v, want EACCES", err)
	}
}

// R022: a hard-linked kubeconfig is rewritten in place, keeping the link;
// a 0400/0600 mode is kept on the atomic path.
func TestSaveConfigKeepsHardLinkAndStrictMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	link := filepath.Join(dir, "config-link")
	writeKubeconfig(t, path, existingConfig)
	if err := os.Link(path, link); err != nil {
		t.Skip("hard links unsupported:", err)
	}
	if err := NewSwitcherWithPath(path).SetCurrentContext("prod"); err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.LoadFromFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CurrentContext != "prod" {
		t.Error("hard link split: other name still has the old content")
	}

	strict := filepath.Join(dir, "strict")
	writeKubeconfig(t, strict, existingConfig)
	if err := os.Chmod(strict, 0400); err != nil {
		t.Fatal(err)
	}
	if err := NewSwitcherWithPath(strict).SetCurrentContext("prod"); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(strict); info.Mode().Perm() != 0400 {
		t.Errorf("mode = %v, want 0400 kept", info.Mode().Perm())
	}
}
