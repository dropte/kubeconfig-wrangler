package profile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, keySize) }

func newTestStore(t *testing.T, path string, key []byte) *Store {
	t.Helper()
	s, err := NewStoreWithOptions(path, NewEncryptorWithKey(key))
	if err != nil {
		t.Fatalf("NewStoreWithOptions: %v", err)
	}
	return s
}

func rancherReq() *ProfileCreateRequest {
	return &ProfileCreateRequest{
		Name: "r", Type: ProfileTypeRancher, RancherURL: "https://rancher.example", Token: "tok-secret",
	}
}

const staticKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c1
  cluster: {server: "https://c1:6443"}
users:
- name: u1
  user: {token: abc}
contexts:
- name: ctx1
  context: {cluster: c1, user: u1}
- name: ctx2
  context: {cluster: c1, user: u1}
`

func TestStoreRoundTripEncrypted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	s := newTestStore(t, path, testKey(1))

	p, err := s.Create(rancherReq())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetClusterAlias(p.ID, "c-1", "prod"); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "tok-secret") {
		t.Fatal("token stored in plaintext")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}

	s2 := newTestStore(t, path, testKey(1))
	got, err := s2.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "tok-secret" || got.ClusterAliases["c-1"] != "prod" || got.Locked {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if len(s2.LoadWarnings()) != 0 {
		t.Fatalf("unexpected warnings: %v", s2.LoadWarnings())
	}
}

func TestGetReturnsCopy(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "profiles.json"), testKey(1))
	p, _ := s.Create(rancherReq())
	_ = s.SetClusterAlias(p.ID, "c", "a")

	got, _ := s.Get(p.ID)
	got.ClusterAliases["c"] = "mutated"
	got.Name = "mutated"
	if again, _ := s.Get(p.ID); again.ClusterAliases["c"] != "a" || again.Name != "r" {
		t.Fatal("Get returned an internal pointer")
	}
	s.List()[0].ClusterAliases["c"] = "mutated"
	if s.GetClusterAlias(p.ID, "c") != "a" {
		t.Fatal("List returned an internal map")
	}
}

func TestConcurrentAliasAndList(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "profiles.json"), testKey(1))
	p, _ := s.Create(rancherReq())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = s.SetClusterAlias(p.ID, "c", strings.Repeat("x", i+1))
			_ = s.SetExportedContext(p.ID, "c", "ctx")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			for _, lp := range s.List() {
				_, _ = json.Marshal(lp)
				_ = lp.ClusterAliases["c"]
			}
		}
	}()
	wg.Wait()
}

func TestUpdatePreservesServerStateAndSecrets(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "profiles.json"), testKey(1))
	p, _ := s.Create(rancherReq())
	_ = s.SetClusterAlias(p.ID, "c-1", "prod")
	_ = s.SetExportedContext(p.ID, "c-1", "prod")

	// Edit form re-submits without token, aliases or exports. (Enabling
	// Skip TLS would require re-entering the token; see
	// TestUpdateRancherEndpointChangeRequiresSecret.)
	got, err := s.Update(p.ID, &ProfileCreateRequest{
		Name: "renamed", Type: ProfileTypeRancher, RancherURL: "https://rancher.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "tok-secret" {
		t.Errorf("token not kept: %q", got.Token)
	}
	if got.ClusterAliases["c-1"] != "prod" || got.ExportedContexts["c-1"] != "prod" {
		t.Errorf("aliases/exports lost: %+v", got)
	}
	if !got.CreatedAt.Equal(p.CreatedAt) || got.Name != "renamed" {
		t.Errorf("unexpected merge result: %+v", got)
	}

	// An explicit empty alias map clears aliases; exports stay.
	got, _ = s.Update(p.ID, &ProfileCreateRequest{
		Name: "renamed", Type: ProfileTypeRancher, RancherURL: "https://rancher.example",
		ClusterAliases: map[string]string{},
	})
	if len(got.ClusterAliases) != 0 || got.ExportedContexts["c-1"] != "prod" {
		t.Errorf("explicit empty aliases: %+v", got)
	}

	// Switching to username/password drops the token.
	got, err = s.Update(p.ID, &ProfileCreateRequest{
		Name: "renamed", Type: ProfileTypeRancher, RancherURL: "https://rancher.example",
		Username: "admin", Password: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "" || got.Password != "pw" {
		t.Errorf("switch to password: %+v", got)
	}

	// Same username, empty password keeps the password.
	got, _ = s.Update(p.ID, &ProfileCreateRequest{
		Name: "renamed", Type: ProfileTypeRancher, RancherURL: "https://rancher.example", Username: "admin",
	})
	if got.Password != "pw" {
		t.Errorf("password not kept: %q", got.Password)
	}

	// Different username without a password fails validation (password not carried over).
	if _, err := s.Update(p.ID, &ProfileCreateRequest{
		Name: "renamed", Type: ProfileTypeRancher, RancherURL: "https://rancher.example", Username: "other",
	}); err == nil {
		t.Error("expected validation error for new username without password")
	}

	// Switching back to a token clears the password.
	got, _ = s.Update(p.ID, &ProfileCreateRequest{
		Name: "renamed", Type: ProfileTypeRancher, RancherURL: "https://rancher.example", Token: "new-tok",
	})
	if got.Password != "" || got.Token != "new-tok" {
		t.Errorf("switch to token: %+v", got)
	}
}

func TestUpdateEKSSecrets(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "profiles.json"), testKey(1))
	p, err := s.Create(&ProfileCreateRequest{
		Name: "e", Type: ProfileTypeEKS, AWSRegion: "us-east-1",
		AccessKey: "AK1", SecretKey: "SK1", SessionToken: "ST1",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.Update(p.ID, &ProfileCreateRequest{Name: "e", Type: ProfileTypeEKS, AWSRegion: "eu-west-1", AccessKey: "AK1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.SecretKey != "SK1" || got.SessionToken != "ST1" {
		t.Errorf("secrets not kept: %+v", got)
	}

	got, err = s.Update(p.ID, &ProfileCreateRequest{Name: "e", Type: ProfileTypeEKS, AWSRegion: "eu-west-1", AWSProfile: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if got.SecretKey != "" || got.SessionToken != "" {
		t.Errorf("switching to AWS profile should clear secrets: %+v", got)
	}
}

func TestUpdateStaticKeepsKubeconfig(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), "profiles.json"), testKey(1))
	p, err := s.Create(&ProfileCreateRequest{Name: "s", Type: ProfileTypeStatic, Kubeconfig: staticKubeconfig})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Update(p.ID, &ProfileCreateRequest{Name: "s2", Type: ProfileTypeStatic})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kubeconfig != staticKubeconfig {
		t.Error("kubeconfig not kept")
	}
	v := got.View()
	if !v.HasKubeconfig || v.KubeconfigContexts != 2 || v.Id != p.ID {
		t.Errorf("unexpected view: %+v", v)
	}
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), "token: abc") {
		t.Error("view leaks kubeconfig content")
	}
}

func TestLockedProfilesSurviveWrongKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")

	sA := newTestStore(t, path, testKey(1))
	locked, _ := sA.Create(rancherReq())
	_ = sA.SetClusterAlias(locked.ID, "c-1", "prod")
	original, _ := os.ReadFile(path)

	var onDisk storeData
	_ = json.Unmarshal(original, &onDisk)
	encryptedToken := onDisk.Profiles[0].Token

	sB := newTestStore(t, path, testKey(2))
	if w := sB.LoadWarnings(); len(w) != 1 || !strings.Contains(w[0], "could not be decrypted") {
		t.Fatalf("warnings = %v", w)
	}

	list := sB.List()
	if len(list) != 1 || !list[0].Locked || list[0].Token != "" || list[0].ClusterAliases["c-1"] != "prod" {
		t.Fatalf("locked profile not surfaced correctly: %+v", list)
	}
	if !list[0].View().Locked {
		t.Error("View() lost Locked")
	}

	// A save triggered by another profile keeps the locked one verbatim and backs up first.
	if _, err := sB.Create(&ProfileCreateRequest{Name: "new", Type: ProfileTypeStatic, Kubeconfig: staticKubeconfig}); err != nil {
		t.Fatal(err)
	}
	var after storeData
	data, _ := os.ReadFile(path)
	_ = json.Unmarshal(data, &after)
	found := false
	for _, p := range after.Profiles {
		if p.ID == locked.ID {
			found = p.Token == encryptedToken
		}
	}
	if !found || len(after.Profiles) != 2 {
		t.Fatalf("locked profile not written back verbatim: %s", data)
	}
	backups, _ := filepath.Glob(path + ".*.bak")
	if len(backups) != 1 {
		t.Fatalf("backups = %v", backups)
	}
	if b, _ := os.ReadFile(backups[0]); !bytes.Equal(b, original) {
		t.Error("backup does not match original file")
	}

	// Alias changes on a locked profile are refused.
	if err := sB.SetClusterAlias(locked.ID, "c-1", "x"); err == nil {
		t.Error("expected error setting alias on locked profile")
	}

	// Update re-enters credentials, re-encrypts with key B and unlocks.
	got, err := sB.Update(locked.ID, &ProfileCreateRequest{
		Name: "r", Type: ProfileTypeRancher, RancherURL: "https://rancher.example", Token: "fresh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Locked || got.ClusterAliases["c-1"] != "prod" {
		t.Errorf("unexpected update result: %+v", got)
	}
	sB2 := newTestStore(t, path, testKey(2))
	if len(sB2.LoadWarnings()) != 0 {
		t.Fatalf("still locked after update: %v", sB2.LoadWarnings())
	}
	if p, _ := sB2.Get(locked.ID); p.Token != "fresh" {
		t.Errorf("token = %q", p.Token)
	}
}

func TestDeleteLockedProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	sA := newTestStore(t, path, testKey(1))
	p, _ := sA.Create(rancherReq())

	sB := newTestStore(t, path, testKey(2))
	if err := sB.Delete(p.ID); err != nil {
		t.Fatal(err)
	}
	if len(newTestStore(t, path, testKey(2)).List()) != 0 {
		t.Error("locked profile not deleted")
	}
}

func TestKeyRegenerationRefusedOverEncryptedStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	keyFile := filepath.Join(dir, keyFileName)

	// No store yet: a key file is created with 0600.
	key, err := getOrCreateFileKey(keyFile, path)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(keyFile); info.Mode().Perm() != 0600 {
		t.Fatalf("key file mode = %v", info.Mode().Perm())
	}
	again, err := getOrCreateFileKey(keyFile, path)
	if err != nil || !bytes.Equal(key, again) {
		t.Fatal("key file not reused")
	}

	// A store with only non-secret data does not block regeneration.
	_ = os.WriteFile(path, []byte(`{"version":1,"profiles":[{"id":"x","name":"x","type":"eks","awsProfile":"p"}]}`), 0600)
	if has, err := storeHasEncryptedData(path); err != nil || has {
		t.Fatalf("has=%v err=%v", has, err)
	}

	s := newTestStore(t, path, key)
	if _, err := s.Create(rancherReq()); err != nil {
		t.Fatal(err)
	}
	if has, err := storeHasEncryptedData(path); err != nil || !has {
		t.Fatalf("has=%v err=%v", has, err)
	}

	// Missing key file with encrypted data: refuse.
	_ = os.Remove(keyFile)
	if _, err := getOrCreateFileKey(keyFile, path); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("expected refusal, got %v", err)
	}
	if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
		t.Error("key file was created despite refusal")
	}

	// Wrong-length key file with encrypted data: refuse.
	_ = os.WriteFile(keyFile, []byte("c2hvcnQ="), 0600)
	if _, err := getOrCreateFileKey(keyFile, path); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("expected refusal, got %v", err)
	}
}

func TestGetOrCreateKeyFileModeFromEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(keyringEnv, "file")
	key, err := getOrCreateKey(dir, filepath.Join(dir, profilesFile))
	if err != nil || len(key) != keySize {
		t.Fatalf("key=%d err=%v", len(key), err)
	}
	if _, err := os.Stat(filepath.Join(dir, keyFileName)); err != nil {
		t.Fatal(err)
	}
}

func TestConfigDirEnvOverride(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv(configDirEnv, dir)
	got, err := ConfigDir()
	if err != nil || got != dir {
		t.Fatalf("ConfigDir = %q, %v", got, err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatal("config dir not created")
	}
}
