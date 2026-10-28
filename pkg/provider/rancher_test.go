package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeRancher records requests and serves login, cluster list and logout.
type fakeRancher struct {
	mu       sync.Mutex
	requests []string
	auth     []string
}

func (f *fakeRancher) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()

	switch {
	case r.URL.Path == "/v3-public/localProviders/local":
		var req loginRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Password != "good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(loginResponse{Token: "token-abc:xyz"})
	case r.URL.Path == "/v3/clusters":
		_ = json.NewEncoder(w).Encode(clusterCollection{Data: []rancherCluster{{ID: "c-1", Name: "one", State: "active"}}})
	case r.URL.Path == "/v3/tokens" && r.URL.Query().Get("action") == "logout":
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeRancher) snapshot() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...), append([]string(nil), f.auth...)
}

func TestRancherProvider_TrailingSlashAndPasswordLogout(t *testing.T) {
	f := &fakeRancher{}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()

	p, err := NewRancherProvider(RancherConfig{URL: srv.URL + "//", Username: "admin", Password: "good"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListClusters(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	_ = p.Close() // second Close must not log out again

	reqs, auth := f.snapshot()
	want := []string{
		"POST /v3-public/localProviders/local?action=login",
		"GET /v3/clusters",
		"POST /v3/tokens?action=logout",
	}
	if strings.Join(reqs, "|") != strings.Join(want, "|") {
		t.Errorf("requests = %v, want %v", reqs, want)
	}
	if auth[len(auth)-1] != "Bearer token-abc:xyz" {
		t.Errorf("logout auth = %q", auth[len(auth)-1])
	}
}

func TestRancherProvider_PrefersTokenOverPassword(t *testing.T) {
	f := &fakeRancher{}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()

	// The password is wrong; the token must be used without attempting login.
	p, err := NewRancherProvider(RancherConfig{URL: srv.URL, Token: "token-1:secret", Username: "admin", Password: "bad"})
	if err != nil {
		t.Fatalf("expected token auth to be used, got %v", err)
	}
	if _, err := p.ListClusters(); err != nil {
		t.Fatal(err)
	}
	p.Close()

	reqs, _ := f.snapshot()
	for _, r := range reqs {
		if strings.Contains(r, "login") || strings.Contains(r, "logout") {
			t.Errorf("unexpected request %q with token auth", r)
		}
	}
}

func TestRancherProvider_LoginFailureHint(t *testing.T) {
	f := &fakeRancher{}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()

	_, err := NewRancherProvider(RancherConfig{URL: srv.URL, Username: "admin", Password: "bad"})
	if err == nil || !strings.Contains(err.Error(), "local accounts") || !strings.Contains(err.Error(), "API token") {
		t.Errorf("error = %v, want hint about local accounts and API tokens", err)
	}
}

func TestRancherProvider_TransportUsesProxyFromEnvironment(t *testing.T) {
	p, err := NewRancherProvider(RancherConfig{URL: "https://rancher.example.com", Token: "a:b"})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := p.httpClient.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Error("transport must honour HTTP(S)_PROXY")
	}
}

// strictRancher only accepts basic auth "good-ak:good-sk" or the bearer
// token returned by a password login with password "good".
func strictRancher(t *testing.T, kubeconfig string) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		if r.URL.Path == "/v3-public/localProviders/local" {
			var req loginRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Password != "good" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(loginResponse{Token: "session-token"})
			return
		}
		ak, sk, basic := r.BasicAuth()
		if (!basic || ak != "good-ak" || sk != "good-sk") && r.Header.Get("Authorization") != "Bearer session-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"must authenticate"}`))
			return
		}
		switch {
		case r.URL.Path == "/v3/clusters":
			_ = json.NewEncoder(w).Encode(clusterCollection{Data: []rancherCluster{{ID: "c-1", Name: "one", State: "active"}}})
		case r.URL.Path == "/v3/clusters/c-1" && r.URL.Query().Get("action") == "generateKubeconfig":
			_ = json.NewEncoder(w).Encode(kubeconfigResponse{Config: kubeconfig})
		case r.URL.Path == "/v3/tokens":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func TestRancherProvider_ExpiredTokenFallsBackToPassword(t *testing.T) {
	srv, reqs := strictRancher(t, "apiVersion: v1\nkind: Config\n")

	// A v0.1.5 profile: stale token plus a working username/password.
	p, err := NewRancherProvider(RancherConfig{URL: srv.URL, Token: "stale-ak:stale-sk", Username: "admin", Password: "good"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	clusters, err := p.ListClusters()
	if err != nil {
		t.Fatalf("expected password fallback, got %v", err)
	}
	if len(clusters) != 1 {
		t.Fatalf("clusters = %v", clusters)
	}
	if _, err := p.GetKubeconfig("c-1"); err != nil {
		t.Fatalf("GetKubeconfig after fallback: %v", err)
	}

	logins := 0
	for _, r := range *reqs {
		if strings.Contains(r, "action=login") {
			logins++
		}
	}
	if logins != 1 {
		t.Errorf("logins = %d, want exactly 1 (requests %v)", logins, *reqs)
	}
}

func TestRancherProvider_TokenAndPasswordBothRejected(t *testing.T) {
	srv, _ := strictRancher(t, "x")
	p, err := NewRancherProvider(RancherConfig{URL: srv.URL, Token: "stale-ak:stale-sk", Username: "admin", Password: "bad"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.ListClusters()
	if err == nil || !strings.Contains(err.Error(), "API token was rejected") || !strings.Contains(err.Error(), "password login also failed") {
		t.Errorf("error = %v, want both credentials reported", err)
	}
}

func TestRancherProvider_TokenOnlyRejectedNamesCredential(t *testing.T) {
	srv, _ := strictRancher(t, "x")
	p, err := NewRancherProvider(RancherConfig{URL: srv.URL, Token: "stale-ak:stale-sk"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.ListClusters()
	if err == nil || !strings.Contains(err.Error(), "rejected credential: API token") {
		t.Errorf("error = %v, want rejected credential note", err)
	}
}

func TestRancherProvider_MalformedTokenWithPasswordUsesLogin(t *testing.T) {
	srv, _ := strictRancher(t, "x")
	p, err := NewRancherProvider(RancherConfig{URL: srv.URL, Token: "not-a-pair", Username: "admin", Password: "good"})
	if err != nil {
		t.Fatalf("expected password login, got %v", err)
	}
	defer p.Close()
	if _, err := p.ListClusters(); err != nil {
		t.Fatal(err)
	}
}

func TestRancherProvider_GetKubeconfigEmptyConfigIsError(t *testing.T) {
	for _, body := range []string{"", "   \n"} {
		srv, _ := strictRancher(t, body)
		p, err := NewRancherProvider(RancherConfig{URL: srv.URL, Token: "good-ak:good-sk"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.GetKubeconfig("c-1")
		if err == nil || !strings.Contains(err.Error(), "empty kubeconfig for cluster c-1") {
			t.Errorf("config %q: error = %v, want empty kubeconfig error", body, err)
		}
	}
}

func TestRancherProvider_GetKubeconfigEmptyObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	p, err := NewRancherProvider(RancherConfig{URL: srv.URL, Token: "a:b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetKubeconfig("c-9"); err == nil {
		t.Error("expected an error for a 200 response with no config")
	}
}

func TestRancherProvider_RedirectIsNotFollowed(t *testing.T) {
	var gets int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			mu.Lock()
			gets++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"config":"apiVersion: v1"}`))
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	p, err := NewRancherProvider(RancherConfig{URL: srv.URL, Token: "a:b"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.GetKubeconfig("c-1")
	if err == nil || !strings.Contains(err.Error(), "status 302") {
		t.Errorf("error = %v, want status 302", err)
	}
	if gets != 0 {
		t.Errorf("redirect was followed %d times", gets)
	}
}
