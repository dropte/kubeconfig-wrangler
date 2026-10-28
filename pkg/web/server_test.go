package web

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	kctx "github.com/kubeconfig-wrangler/pkg/context"
	"github.com/kubeconfig-wrangler/pkg/kubeconfig"
	"github.com/kubeconfig-wrangler/pkg/profile"
	"github.com/kubeconfig-wrangler/pkg/provider"
)

const (
	testAddr  = "127.0.0.1:18080"
	testHost  = "127.0.0.1:18080"
	testToken = "test-token-123"
)

// newServerForTest builds a server with an in-memory-keyed store and a
// switcher pointed at switcherPath; no keychain or home directory is used.
func newServerForTest(addr, token string, store *profile.Store, switcherPath string) *Server {
	return newServer(addr, token, store, nil, kctx.NewSwitcherWithPath(switcherPath))
}

type testEnv struct {
	srv            *Server
	store          *profile.Store
	kubeconfigPath string
}

func setup(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	key := bytes.Repeat([]byte{7}, 32)
	store, err := profile.NewStoreWithOptions(filepath.Join(dir, "profiles.json"), profile.NewEncryptorWithKey(key))
	if err != nil {
		t.Fatalf("NewStoreWithOptions: %v", err)
	}
	kcPath := filepath.Join(dir, "kube", "config")
	if err := os.MkdirAll(filepath.Dir(kcPath), 0o700); err != nil {
		t.Fatal(err)
	}
	return &testEnv{
		srv:            newServerForTest(testAddr, testToken, store, kcPath),
		store:          store,
		kubeconfigPath: kcPath,
	}
}

// do sends a request through the full middleware chain. Headers are given as
// key/value pairs.
func (e *testEnv) do(method, target, body string, headers ...string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	req.Host = testHost
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			req.Host = headers[i+1]
			continue
		}
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// api sends an authenticated JSON API request.
func (e *testEnv) api(method, target, body string) *httptest.ResponseRecorder {
	return e.do(method, target, body, "X-Auth-Token", testToken, "Content-Type", "application/json")
}

func TestHostHeaderRejected(t *testing.T) {
	e := setup(t)
	for _, host := range []string{"evil.example:18080", "127.0.0.1:9999", "localhost"} {
		rec := e.do("GET", "/?token="+testToken, "", "Host", host)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Host %q: got %d, want 403", host, rec.Code)
		}
	}
	for _, host := range []string{"localhost:18080", "[::1]:18080", "LOCALHOST:18080"} {
		rec := e.do("GET", "/api/status", "", "Host", host, "X-Auth-Token", testToken)
		if rec.Code != http.StatusOK {
			t.Errorf("Host %q: got %d, want 200", host, rec.Code)
		}
	}
}

func TestAllowedHostsNonLoopbackBind(t *testing.T) {
	hosts := allowedHosts("192.168.1.10:8080")
	for _, h := range []string{"192.168.1.10:8080", "127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		if !hosts[h] {
			t.Errorf("expected %q to be allowed", h)
		}
	}
	if hosts["192.168.1.10:9090"] || hosts["evil.example:8080"] {
		t.Error("unexpected host allowed")
	}
}

func TestIndexRequiresToken(t *testing.T) {
	e := setup(t)

	rec := e.do("GET", "/", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), testToken) {
		t.Fatal("401 page leaks the token")
	}

	if rec := e.do("GET", "/?token=wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", rec.Code)
	}

	rec = e.do("GET", "/?token="+testToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("query token: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `<meta name="auth-token" content="`+testToken+`">`) {
		t.Fatal("page does not contain the auth-token meta tag")
	}

	if rec := e.do("GET", "/", "", "X-Auth-Token", testToken); rec.Code != http.StatusOK {
		t.Fatalf("header token: got %d, want 200", rec.Code)
	}
}

func TestAPIRequiresToken(t *testing.T) {
	e := setup(t)
	if rec := e.do("GET", "/api/profiles", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no header: got %d, want 401", rec.Code)
	}
	if rec := e.do("GET", "/api/profiles", "", "X-Auth-Token", "nope"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad header: got %d, want 401", rec.Code)
	}
	if rec := e.do("GET", "/api/profiles", "", "X-Auth-Token", testToken); rec.Code != http.StatusOK {
		t.Fatalf("good header: got %d, want 200", rec.Code)
	}
}

func TestEmptyTokenNeverMatches(t *testing.T) {
	e := setup(t)
	e.srv.token = ""
	if rec := e.do("GET", "/api/profiles", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
	if err := e.srv.Start(); err == nil {
		t.Fatal("Start with empty token should fail")
	}
}

func TestNonGETRequiresJSON(t *testing.T) {
	e := setup(t)
	body := `{"name":"x","type":"static","kubeconfig":"x"}`
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", ""} {
		rec := e.do("POST", "/api/profiles", body, "X-Auth-Token", testToken, "Content-Type", ct)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q: got %d, want 415", ct, rec.Code)
		}
	}
	if len(e.store.List()) != 0 {
		t.Fatal("profile was created despite rejected content type")
	}
	rec := e.do("POST", "/api/context/delete", `{"name":"x"}`, "X-Auth-Token", testToken, "Content-Type", "application/json; charset=utf-8")
	if rec.Code == http.StatusUnsupportedMediaType {
		t.Fatal("application/json with parameters was rejected")
	}
}

func TestBodylessDeleteAllowed(t *testing.T) {
	e := setup(t)
	rec := e.do("DELETE", "/api/profiles/missing", "", "X-Auth-Token", testToken)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 from handler", rec.Code)
	}
}

func TestOriginAndFetchSiteChecks(t *testing.T) {
	e := setup(t)
	cases := []struct {
		name    string
		headers []string
		want    int
	}{
		{"other localhost port", []string{"Origin", "http://localhost:3000"}, http.StatusForbidden},
		{"foreign origin", []string{"Origin", "http://evil.example"}, http.StatusForbidden},
		{"cross-site fetch", []string{"Sec-Fetch-Site", "cross-site"}, http.StatusForbidden},
		{"same-site fetch", []string{"Sec-Fetch-Site", "same-site"}, http.StatusForbidden},
		{"same origin", []string{"Origin", "http://" + testHost, "Sec-Fetch-Site", "same-origin"}, http.StatusBadRequest},
	}
	for _, c := range cases {
		h := append([]string{"X-Auth-Token", testToken, "Content-Type", "application/json"}, c.headers...)
		rec := e.do("POST", "/api/context/rename", `{}`, h...)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.want)
		}
	}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, data interface{}) APIResponse {
	t.Helper()
	resp := APIResponse{Data: data}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return resp
}

func TestProfilesAreRedacted(t *testing.T) {
	e := setup(t)
	rec := e.api("POST", "/api/profiles", `{"name":"r","type":"rancher","rancherUrl":"https://rancher.example","token":"token-abc:SECRET"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("create response leaks the token: %s", rec.Body.String())
	}
	var created profile.ProfileView
	decode(t, rec, &created)

	for _, target := range []string{"/api/profiles", "/api/profiles/" + created.Id} {
		rec := e.api("GET", target, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d", target, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, "SECRET") || strings.Contains(body, `"token"`) {
			t.Fatalf("GET %s leaks secrets: %s", target, body)
		}
		if !strings.Contains(body, `"hasToken":true`) {
			t.Fatalf("GET %s missing hasToken: %s", target, body)
		}
	}
}

func TestStoreUnavailable(t *testing.T) {
	e := setup(t)
	srv := newServer(testAddr, testToken, nil, os.ErrPermission, kctx.NewSwitcherWithPath(e.kubeconfigPath))
	req := httptest.NewRequest("GET", "/api/profiles", nil)
	req.Host = testHost
	req.Header.Set("X-Auth-Token", testToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "profile store unavailable: permission denied") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest("GET", "/api/status", nil)
	req.Host = testHost
	req.Header.Set("X-Auth-Token", testToken)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var status struct {
		StoreAvailable bool   `json:"storeAvailable"`
		StoreError     string `json:"storeError"`
		KubeconfigPath string `json:"kubeconfigPath"`
	}
	decode(t, rec, &status)
	if status.StoreAvailable || status.StoreError != "permission denied" || status.KubeconfigPath != e.kubeconfigPath {
		t.Fatalf("unexpected status %+v", status)
	}
}

const staticKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: dev
  cluster:
    server: https://dev.example:6443
- name: prod
  cluster:
    server: https://prod.example:6443
users:
- name: dev
  user:
    token: dev-token
- name: prod
  user:
    token: prod-token
contexts:
- name: dev
  context:
    cluster: dev
    user: dev
- name: prod
  context:
    cluster: prod
    user: prod
current-context: dev
`

func createStatic(t *testing.T, e *testEnv, name string) string {
	t.Helper()
	p, err := e.store.Create(&profile.ProfileCreateRequest{Name: name, Type: profile.ProfileTypeStatic, Kubeconfig: staticKubeconfig})
	if err != nil {
		t.Fatalf("create static profile: %v", err)
	}
	return p.ID
}

func TestGenerateMergeModeTracksWrittenContexts(t *testing.T) {
	e := setup(t)
	id := createStatic(t, e, "Static One")

	// An existing kubeconfig with its own current-context, which must be kept.
	existing := `apiVersion: v1
kind: Config
clusters:
- name: other
  cluster:
    server: https://other.example
users:
- name: other
  user: {}
contexts:
- name: other
  context:
    cluster: other
    user: other
current-context: other
`
	if err := os.WriteFile(e.kubeconfigPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	body := `{"profileId":"` + id + `","clusterPrefix":"x-","mergeToKubeconfig":true,
		"selectedClusters":[{"id":"dev","name":"dev"},{"id":"missing","name":"missing"}]}`
	rec := e.api("POST", "/api/generate/profile", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("merge: %d %s", rec.Code, rec.Body.String())
	}
	var result MergeResult
	resp := decode(t, rec, &result)
	if !resp.Success {
		t.Fatalf("merge not successful: %s", rec.Body.String())
	}
	if len(result.Added) != 1 || result.Added[0] != "x-dev" {
		t.Fatalf("added = %v, want [x-dev]", result.Added)
	}
	if len(result.Failed) != 1 || result.Failed[0].Cluster != "missing" {
		t.Fatalf("failed = %+v, want the missing cluster", result.Failed)
	}

	data, err := os.ReadFile(e.kubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "x-dev") || !strings.Contains(string(data), "current-context: other") {
		t.Fatalf("kubeconfig not merged as expected:\n%s", data)
	}

	if got := e.store.GetExportedContext(id, "dev"); got != "x-dev" {
		t.Fatalf("exported context for dev = %q, want x-dev", got)
	}
	if got := e.store.GetExportedContext(id, "missing"); got != "" {
		t.Fatalf("failed cluster was tracked as %q", got)
	}
}

func TestGenerateAllDisambiguatesDuplicateNames(t *testing.T) {
	e := setup(t)
	a := createStatic(t, e, "Alpha")
	b := createStatic(t, e, "Beta")

	body := `{"mergeToKubeconfig":true,"selectedClusters":[
		{"id":"dev","name":"dev","profileId":"` + a + `"},
		{"id":"dev","name":"dev","profileId":"` + b + `"}]}`
	rec := e.api("POST", "/api/generate/all", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate: %d %s", rec.Code, rec.Body.String())
	}
	var result MergeResult
	decode(t, rec, &result)
	if strings.Join(result.Added, ",") != "dev,dev-beta" {
		t.Fatalf("added = %v, want [dev dev-beta]", result.Added)
	}
	if e.store.GetExportedContext(a, "dev") != "dev" || e.store.GetExportedContext(b, "dev") != "dev-beta" {
		t.Fatalf("tracking: a=%q b=%q", e.store.GetExportedContext(a, "dev"), e.store.GetExportedContext(b, "dev"))
	}
}

func TestGenerateProfileDisambiguatesAliasCollision(t *testing.T) {
	e := setup(t)
	id := createStatic(t, e, "S")
	body := `{"profileId":"` + id + `","selectedClusters":[{"id":"dev","name":"dev"},{"id":"prod","name":"prod","alias":"dev"}]}`
	rec := e.api("POST", "/api/generate/profile", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate: %d %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "name: dev\n") || !strings.Contains(out, "name: dev-2\n") {
		t.Fatalf("expected both dev and dev-2 contexts:\n%s", out)
	}
}

func TestDownloadPartialFailureSetsWarningsHeader(t *testing.T) {
	e := setup(t)
	id := createStatic(t, e, "S")
	body := `{"profileId":"` + id + `","selectedClusters":[{"id":"dev","name":"dev"},{"id":"gone","name":"gone ü"}]}`
	rec := e.api("POST", "/api/generate/profile", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate: %d %s", rec.Code, rec.Body.String())
	}
	raw := rec.Header().Get(warningsHeader)
	var warnings []string
	if err := json.Unmarshal([]byte(raw), &warnings); err != nil {
		t.Fatalf("header %q is not JSON: %v", raw, err)
	}
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "gone ü: ") {
		t.Fatalf("warnings = %q", warnings)
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] >= 0x80 {
			t.Fatalf("header contains non-ASCII byte: %q", raw)
		}
	}
}

// TestLegacyRoutesRemoved: the credential-forwarding legacy endpoints and the
// raw kubeconfig merge endpoint are gone (R009).
func TestLegacyRoutesRemoved(t *testing.T) {
	e := setup(t)
	kc, _ := json.Marshal(staticKubeconfig)
	for path, body := range map[string]string{
		"/api/clusters":      `{"rancherUrl":"https://rancher.example","token":"t"}`,
		"/api/generate":      `{"rancherUrl":"https://rancher.example","token":"t"}`,
		"/api/context/merge": `{"kubeconfig":` + string(kc) + `}`,
	} {
		rec := e.api("POST", path, body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", path, rec.Code)
		}
	}
	if _, err := os.Stat(e.kubeconfigPath); !os.IsNotExist(err) {
		t.Fatalf("kubeconfig was written (stat err %v)", err)
	}
}

func TestListAllClustersAppliesAliasesAndSorts(t *testing.T) {
	e := setup(t)
	b := createStatic(t, e, "Beta")
	a := createStatic(t, e, "Alpha")
	if err := e.store.SetClusterAlias(b, "prod", "production"); err != nil {
		t.Fatal(err)
	}
	rec := e.api("GET", "/api/clusters/all", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var clusters []struct {
		ID, Name, Alias, ProfileID string
	}
	decode(t, rec, &clusters)
	var got []string
	for _, c := range clusters {
		got = append(got, c.Name+"/"+c.Alias)
	}
	want := []string{"dev/", "prod/", "dev/", "prod/production"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("clusters = %v, want %v", got, want)
	}
	if clusters[0].ProfileID != a || clusters[3].ProfileID != b {
		t.Fatal("clusters not sorted by profile name")
	}
}

func TestSanitizeZipName(t *testing.T) {
	cases := map[string]string{
		"arn:aws:eks:us-east-1:123:cluster/prod": "arn-aws-eks-us-east-1-123-cluster-prod",
		`a<b>c"d\e|f?g*h`:                        "a-b-c-d-e-f-g-h",
		"tab\there":                              "tab-here",
		"..hidden. ":                             "hidden",
		"...":                                    "cluster",
		"con":                                    "con-",
		"prod":                                   "prod",
	}
	for in, want := range cases {
		if got := sanitizeZipName(in); got != want {
			t.Errorf("sanitizeZipName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildZipUniqueEntries(t *testing.T) {
	gen := kubeconfig.NewGenerator("")
	single := func(name string) string {
		return strings.ReplaceAll(strings.ReplaceAll(staticKubeconfig, "current-context: dev", "current-context: "+name), "prod", "p")
	}
	data, err := buildZip(gen, map[string]string{"a:b": single("dev"), "a/b": single("dev")})
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "a-b.yaml,a-b-2.yaml" {
		t.Fatalf("zip entries = %v", names)
	}
}

// TestProfileSecretsTrimmedExceptPassword: token, AWS secret key and session
// token lose surrounding whitespace; the password is kept as sent (R019).
func TestProfileSecretsTrimmedExceptPassword(t *testing.T) {
	e := setup(t)
	rec := e.api("POST", "/api/profiles", `{"name":"R","type":"rancher","rancherUrl":"https://rancher.example","token":" tok-1:abc \n","username":"u","password":" pw "}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create rancher: %d %s", rec.Code, rec.Body.String())
	}
	var view struct{ ID string }
	decode(t, rec, &view)
	p, err := e.store.Get(view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Token != "tok-1:abc" || p.Password != " pw " {
		t.Fatalf("token %q password %q", p.Token, p.Password)
	}

	rec = e.api("PUT", "/api/profiles/"+view.ID, `{"name":"R","type":"rancher","rancherUrl":"https://rancher.example","token":"\ttok-2\n","username":"u"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update rancher: %d %s", rec.Code, rec.Body.String())
	}
	if p, _ = e.store.Get(view.ID); p.Token != "tok-2" {
		t.Fatalf("updated token %q", p.Token)
	}

	rec = e.api("POST", "/api/profiles", `{"name":"E","type":"eks","awsRegion":"us-east-1","accessKey":"AKIA","secretKey":" sk \n","sessionToken":" st "}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create eks: %d %s", rec.Code, rec.Body.String())
	}
	decode(t, rec, &view)
	p, err = e.store.Get(view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.SecretKey != "sk" || p.SessionToken != "st" {
		t.Fatalf("secretKey %q sessionToken %q", p.SecretKey, p.SessionToken)
	}
}

// writeOCIConfig writes a throwaway OCI config file (with a generated API
// key) into a temp dir and returns its path. Nothing under ~/.oci is used.
func writeOCIConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "key.pem")
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	section := func(name string) string {
		return "[" + name + "]\n" +
			"user=ocid1.user.oc1..aaaa\n" +
			"fingerprint=11:22:33:44:55:66:77:88:99:00:aa:bb:cc:dd:ee:ff\n" +
			"tenancy=ocid1.tenancy.oc1..bbbb\n" +
			"region=us-ashburn-1\n" +
			"key_file=" + keyPath + "\n\n"
	}
	cfgPath := filepath.Join(dir, "config")
	if err := os.WriteFile(cfgPath, []byte(section("DEFAULT")+section("work")+section("alpha")), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func TestOCIEndpointsRequireToken(t *testing.T) {
	e := setup(t)
	t.Setenv("OCI_CLI_CONFIG_FILE", filepath.Join(t.TempDir(), "missing"))
	for _, target := range []string{"/api/oci/profiles", "/api/oci/regions"} {
		if rec := e.do("GET", target, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without token: got %d, want 401", target, rec.Code)
		}
		if rec := e.do("GET", target, "", "X-Auth-Token", "nope"); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with bad token: got %d, want 401", target, rec.Code)
		}
		if rec := e.api("POST", target, `{}`); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: got %d, want 405", target, rec.Code)
		}
	}
}

func TestOCIProfilesEndpoint(t *testing.T) {
	e := setup(t)
	t.Setenv("OCI_CLI_CONFIG_FILE", writeOCIConfig(t))
	rec := e.api("GET", "/api/oci/profiles", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var profiles []string
	if resp := decode(t, rec, &profiles); !resp.Success {
		t.Fatalf("not successful: %s", rec.Body.String())
	}
	want := []string{"DEFAULT", "alpha", "work"}
	if strings.Join(profiles, ",") != strings.Join(want, ",") {
		t.Fatalf("profiles = %v, want %v", profiles, want)
	}

	// A missing config file yields an empty list, not null or an error.
	t.Setenv("OCI_CLI_CONFIG_FILE", filepath.Join(t.TempDir(), "missing"))
	rec = e.api("GET", "/api/oci/profiles", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("missing file: got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("missing file: want empty data array, got %s", rec.Body.String())
	}
}

func TestOCIRegionsEndpoint(t *testing.T) {
	e := setup(t)
	rec := e.api("GET", "/api/oci/regions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var regions []string
	if resp := decode(t, rec, &regions); !resp.Success {
		t.Fatalf("not successful: %s", rec.Body.String())
	}
	if !slices.IsSorted(regions) {
		t.Fatalf("regions not sorted: %v", regions)
	}
	for _, r := range []string{"us-ashburn-1", "us-phoenix-1", "eu-frankfurt-1", "ap-tokyo-1"} {
		if !slices.Contains(regions, r) {
			t.Errorf("regions missing %s", r)
		}
	}
}

func TestCreateOKEProfile(t *testing.T) {
	e := setup(t)
	body := `{"name":"oci","type":"oke","ociProfile":"work","ociConfigFile":"/tmp/oci/config",` +
		`"ociRegion":"eu-frankfurt-1","ociCompartment":"ocid1.compartment.oc1..cccc",` +
		`"ociAuth":"security_token","okeEndpoint":"private"}`
	rec := e.api("POST", "/api/profiles", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var v profile.ProfileView
	decode(t, rec, &v)
	if v.Type != profile.ProfileTypeOKE || v.OCIProfile != "work" || v.OCIConfigFile != "/tmp/oci/config" ||
		v.OCIRegion != "eu-frankfurt-1" || v.OCICompartment != "ocid1.compartment.oc1..cccc" ||
		v.OCIAuth != profile.OCIAuthSecurityToken || v.OKEEndpoint != profile.OKEEndpointPrivate {
		t.Fatalf("unexpected view: %+v", v)
	}
	if v.HasToken || v.HasPassword || v.HasSecretKey || v.HasSessionToken || v.HasKubeconfig {
		t.Fatalf("OKE profile reports stored secrets: %+v", v)
	}

	rec = e.api("GET", "/api/profiles/"+v.Id, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ociProfile":"work"`) {
		t.Fatalf("GET profile: %d %s", rec.Code, rec.Body.String())
	}

	// Invalid OKE settings are rejected by validation.
	rec = e.api("POST", "/api/profiles", `{"name":"bad","type":"oke","okeEndpoint":"sideways"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid endpoint: got %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestNewProviderForOKEProfile(t *testing.T) {
	cfgPath := writeOCIConfig(t)
	t.Setenv("OCI_CLI_CONFIG_FILE", filepath.Join(t.TempDir(), "missing"))
	p := &profile.Profile{
		ID:            "id-1",
		Name:          "Oracle",
		Type:          profile.ProfileTypeOKE,
		OCIProfile:    "work",
		OCIConfigFile: cfgPath,
		OCIRegion:     "eu-frankfurt-1",
		OKEEndpoint:   profile.OKEEndpointPublic,
	}
	prov, err := newProviderForProfile(p)
	if err != nil {
		t.Fatalf("newProviderForProfile: %v", err)
	}
	defer prov.Close()
	if prov.Type() != "oke" || prov.ProfileID() != "id-1" || prov.Name() != "Oracle" {
		t.Fatalf("unexpected provider: type=%s id=%s name=%s", prov.Type(), prov.ProfileID(), prov.Name())
	}
	if oke, ok := prov.(*provider.OKEProvider); !ok || oke.Region() != "eu-frankfurt-1" || oke.EmbedsCredentials() {
		t.Fatalf("unexpected OKE provider: %#v", prov)
	}

	// The OCI profile and config file are passed through: an unknown profile
	// in the explicit config file is reported against that file.
	p.OCIProfile = "nope"
	if _, err := newProviderForProfile(p); err == nil || !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), cfgPath) {
		t.Fatalf("want unknown-profile error naming %s, got %v", cfgPath, err)
	}
}
