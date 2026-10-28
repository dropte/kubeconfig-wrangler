package profile

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"
)

// fakeKeyring replaces the OS keychain for the duration of a test.
type fakeKeyring struct {
	mu     sync.Mutex
	value  string
	getErr error
	sets   int
}

func installFakeKeyring(t *testing.T, value string, getErr error) *fakeKeyring {
	t.Helper()
	f := &fakeKeyring{value: value, getErr: getErr}
	oldGet, oldSet := keyringGet, keyringSet
	keyringGet = func(service, user string) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.getErr != nil {
			return "", f.getErr
		}
		if f.value == "" {
			return "", keyring.ErrNotFound
		}
		return f.value, nil
	}
	keyringSet = func(service, user, password string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.value = password
		f.sets++
		return nil
	}
	t.Cleanup(func() { keyringGet, keyringSet = oldGet, oldSet })
	t.Setenv(keyringEnv, "")
	return f
}

func (f *fakeKeyring) current() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.value
}

func b64(k []byte) string { return base64.StdEncoding.EncodeToString(k) }

func writeKeyFile(t *testing.T, dir string, key []byte) string {
	t.Helper()
	path := filepath.Join(dir, keyFileName)
	if err := os.WriteFile(path, []byte(b64(key)), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// R004: profiles saved with the fallback key file during a keychain outage
// stay readable once the keychain (holding a different key) is back.
func TestGetOrCreateKeyPrefersFileKeyThatDecryptsStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, profilesFile)
	k1, k2 := testKey(1), testKey(2)

	if _, err := newTestStore(t, path, k2).Create(rancherReq()); err != nil {
		t.Fatal(err)
	}
	writeKeyFile(t, dir, k2)
	kr := installFakeKeyring(t, b64(k1), nil)

	key, err := getOrCreateKeyLocked(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, k2) {
		t.Fatal("keychain key won over the key file that encrypted the store")
	}
	if kr.current() != b64(k2) {
		t.Error("file key not copied into the keychain")
	}
	if _, err := os.Stat(filepath.Join(dir, keyFileName)); err != nil {
		t.Error("key file must not be removed")
	}
	if w := newTestStore(t, path, key).LoadWarnings(); len(w) != 0 {
		t.Fatalf("profiles locked: %v", w)
	}
}

func TestGetOrCreateKeyReconcileCases(t *testing.T) {
	k1, k2 := testKey(1), testKey(2)

	t.Run("no encrypted data prefers file key", func(t *testing.T) {
		dir := t.TempDir()
		writeKeyFile(t, dir, k2)
		kr := installFakeKeyring(t, b64(k1), nil)
		key, err := getOrCreateKey(dir, filepath.Join(dir, profilesFile))
		if err != nil || !bytes.Equal(key, k2) || kr.current() != b64(k2) {
			t.Fatalf("key=%v err=%v keychain=%q", key, err, kr.current())
		}
	})

	t.Run("keychain key decrypts, stale file moved aside", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, profilesFile)
		if _, err := newTestStore(t, path, k1).Create(rancherReq()); err != nil {
			t.Fatal(err)
		}
		keyFile := writeKeyFile(t, dir, k2)
		kr := installFakeKeyring(t, b64(k1), nil)
		key, err := getOrCreateKey(dir, path)
		if err != nil || !bytes.Equal(key, k1) {
			t.Fatalf("key=%v err=%v", key, err)
		}
		if kr.sets != 0 {
			t.Error("keychain must not be overwritten")
		}
		if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
			t.Error("stale key file not moved aside")
		}
		if aside, _ := filepath.Glob(keyFile + ".*.unused"); len(aside) != 1 {
			t.Errorf("aside files = %v", aside)
		}
	})

	t.Run("neither key decrypts", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, profilesFile)
		if _, err := newTestStore(t, path, testKey(3)).Create(rancherReq()); err != nil {
			t.Fatal(err)
		}
		writeKeyFile(t, dir, k2)
		installFakeKeyring(t, b64(k1), nil)
		_, err := getOrCreateKey(dir, path)
		if err == nil || !strings.Contains(err.Error(), "keychain") || !strings.Contains(err.Error(), keyFileName) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("same key in both", func(t *testing.T) {
		dir := t.TempDir()
		writeKeyFile(t, dir, k1)
		kr := installFakeKeyring(t, b64(k1), nil)
		key, err := getOrCreateKey(dir, filepath.Join(dir, profilesFile))
		if err != nil || !bytes.Equal(key, k1) || kr.sets != 0 {
			t.Fatalf("key=%v err=%v sets=%d", key, err, kr.sets)
		}
	})

	t.Run("invalid keychain key uses valid key file", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, profilesFile)
		if _, err := newTestStore(t, path, k2).Create(rancherReq()); err != nil {
			t.Fatal(err)
		}
		writeKeyFile(t, dir, k2)
		kr := installFakeKeyring(t, "not-a-key", nil)
		key, err := getOrCreateKey(dir, path)
		if err != nil || !bytes.Equal(key, k2) || kr.current() != b64(k2) {
			t.Fatalf("key=%v err=%v keychain=%q", key, err, kr.current())
		}
	})

	t.Run("invalid keychain key and non-matching key file refuses", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, profilesFile)
		if _, err := newTestStore(t, path, k1).Create(rancherReq()); err != nil {
			t.Fatal(err)
		}
		writeKeyFile(t, dir, k2)
		kr := installFakeKeyring(t, "not-a-key", nil)
		if _, err := getOrCreateKey(dir, path); err == nil {
			t.Fatal("expected an error")
		}
		if kr.sets != 0 {
			t.Error("keychain must not be overwritten")
		}
	})

	t.Run("keychain error with encrypted data and no key file refuses", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, profilesFile)
		if _, err := newTestStore(t, path, k1).Create(rancherReq()); err != nil {
			t.Fatal(err)
		}
		installFakeKeyring(t, "", errors.New("keychain locked"))
		if _, err := getOrCreateKey(dir, path); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Fatalf("err = %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, keyFileName)); !os.IsNotExist(err) {
			t.Error("key file created over encrypted data")
		}
	})
}

// R013: two processes creating the key at the same time end up with one key.
func TestGetOrCreateKeyLockedSingleKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, profilesFile)
	installFakeKeyring(t, "", nil)

	var wg sync.WaitGroup
	keys := make([][]byte, 8)
	for i := range keys {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k, err := getOrCreateKeyLocked(dir, path)
			if err != nil {
				t.Error(err)
			}
			keys[i] = k
		}(i)
	}
	wg.Wait()
	for _, k := range keys[1:] {
		if !bytes.Equal(k, keys[0]) {
			t.Fatal("concurrent key creation produced different keys")
		}
	}
}

// R013: two Store instances on the same file (two processes) do not lose
// each other's changes.
func TestStoresSharingFileDoNotLoseUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), profilesFile)
	a := newTestStore(t, path, testKey(1))
	b := newTestStore(t, path, testKey(1))

	pa, err := a.Create(rancherReq())
	if err != nil {
		t.Fatal(err)
	}
	pb, err := b.Create(&ProfileCreateRequest{Name: "s", Type: ProfileTypeStatic, Kubeconfig: staticKubeconfig})
	if err != nil {
		t.Fatal(err)
	}
	// b never loaded pa, but can still change it.
	if err := b.SetClusterAlias(pa.ID, "c-1", "prod"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetExportedContext(pb.ID, "c-2", "dev"); err != nil {
		t.Fatal(err)
	}

	fresh := newTestStore(t, path, testKey(1))
	if len(fresh.List()) != 2 {
		t.Fatalf("profiles = %d, want 2", len(fresh.List()))
	}
	if fresh.GetClusterAlias(pa.ID, "c-1") != "prod" || fresh.GetExportedContext(pb.ID, "c-2") != "dev" {
		t.Error("cross-store changes lost")
	}

	// Concurrent creates from separate stores all survive.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := NewStoreWithOptions(path, NewEncryptorWithKey(testKey(1)))
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := s.Create(rancherReq()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len(newTestStore(t, path, testKey(1)).List()); n != 12 {
		t.Fatalf("profiles = %d, want 12", n)
	}
}

// R001: the stored Rancher secret is not carried over to a new endpoint.
func TestUpdateRancherEndpointChangeRequiresSecret(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), profilesFile), testKey(1))
	p, err := s.Create(&ProfileCreateRequest{
		Name: "r", Type: ProfileTypeRancher, RancherURL: "https://rancher.example", Token: "tok-secret",
		Username: "admin", Password: "pw", CACert: "/ca.pem",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := func() *ProfileCreateRequest {
		return &ProfileCreateRequest{
			Name: "r", Type: ProfileTypeRancher, RancherURL: "https://rancher.example", Username: "admin", CACert: "/ca.pem",
		}
	}

	rejected := map[string]func(*ProfileCreateRequest){
		"new URL":         func(r *ProfileCreateRequest) { r.RancherURL = "https://attacker.example" },
		"new URL no user": func(r *ProfileCreateRequest) { r.RancherURL = "https://attacker.example"; r.Username = "" },
		"skip TLS on":     func(r *ProfileCreateRequest) { r.SkipTLS = true },
		"new CA":          func(r *ProfileCreateRequest) { r.CACert = "/other.pem" },
		"CA removed":      func(r *ProfileCreateRequest) { r.CACert = "" },
	}
	for name, mod := range rejected {
		req := base()
		mod(req)
		_, err := s.Update(p.ID, req)
		var ve *ValidationError
		if !errors.As(err, &ve) || !strings.Contains(ve.Message, "re-enter") {
			t.Errorf("%s: err = %v, want re-enter validation error", name, err)
		}
	}
	if got, _ := s.Get(p.ID); got.Token != "tok-secret" || got.Password != "pw" || got.RancherURL != "https://rancher.example" {
		t.Fatalf("rejected update changed the profile: %+v", got)
	}

	// Cosmetic URL differences keep the secrets.
	req := base()
	req.RancherURL = "HTTPS://Rancher.Example/"
	got, err := s.Update(p.ID, req)
	if err != nil || got.Token != "tok-secret" || got.Password != "pw" {
		t.Fatalf("cosmetic URL change: %+v, %v", got, err)
	}

	// A new endpoint with a new token is fine and drops the old password.
	req = base()
	req.RancherURL = "https://new.example"
	req.Token = "tok-new"
	got, err = s.Update(p.ID, req)
	if err != nil || got.Token != "tok-new" || got.Password != "" {
		t.Fatalf("new endpoint with token: %+v, %v", got, err)
	}

	// Enabling Skip TLS then turning it off again keeps the secret.
	req = base()
	req.RancherURL = "https://new.example"
	req.SkipTLS = true
	req.Token = "tok-new"
	if _, err := s.Update(p.ID, req); err != nil {
		t.Fatal(err)
	}
	req.SkipTLS, req.Token = false, ""
	if got, err = s.Update(p.ID, req); err != nil || got.Token != "tok-new" {
		t.Fatalf("disabling Skip TLS: %+v, %v", got, err)
	}
}

// R005: a profile with both a token and username/password keeps the token
// on an edit that re-sends the username only.
func TestUpdateRancherTokenWithUsernameKept(t *testing.T) {
	s := newTestStore(t, filepath.Join(t.TempDir(), profilesFile), testKey(1))

	both, err := s.Create(&ProfileCreateRequest{
		Name: "both", Type: ProfileTypeRancher, RancherURL: "https://rancher.example",
		Token: "tok", Username: "admin", Password: "pw",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Update(both.ID, &ProfileCreateRequest{
		Name: "renamed", Type: ProfileTypeRancher, RancherURL: "https://rancher.example", Username: "admin",
	})
	if err != nil || got.Token != "tok" || got.Password != "pw" || got.Name != "renamed" {
		t.Fatalf("rename: %+v, %v", got, err)
	}

	// A new password switches to username/password.
	got, err = s.Update(both.ID, &ProfileCreateRequest{
		Name: "renamed", Type: ProfileTypeRancher, RancherURL: "https://rancher.example", Username: "admin", Password: "pw2",
	})
	if err != nil || got.Token != "" || got.Password != "pw2" {
		t.Fatalf("new password: %+v, %v", got, err)
	}

	tokUser, err := s.Create(&ProfileCreateRequest{
		Name: "tokuser", Type: ProfileTypeRancher, RancherURL: "https://rancher.example", Token: "tok", Username: "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.Update(tokUser.ID, &ProfileCreateRequest{
		Name: "tokuser", Type: ProfileTypeRancher, RancherURL: "https://rancher.example", Username: "admin", SkipTLS: false,
	})
	if err != nil || got.Token != "tok" {
		t.Fatalf("token+username without password: %+v, %v", got, err)
	}

	// A different username without a password drops the token and fails.
	if _, err := s.Update(tokUser.ID, &ProfileCreateRequest{
		Name: "tokuser", Type: ProfileTypeRancher, RancherURL: "https://rancher.example", Username: "other",
	}); err == nil {
		t.Error("expected validation error for changed username without password")
	}
}
