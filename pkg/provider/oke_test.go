package provider

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/containerengine"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestMain(m *testing.M) {
	// Keep the OCI SDK away from the real ~/.oci developer tool config, and
	// make the tests independent of the caller's OCI_CLI_* environment.
	for k, v := range map[string]string{
		common.OciDeveloperToolConfigurationFilePathEnvVar:    filepath.Join(os.TempDir(), "kw-oke-test-no-such-file.json"),
		"OCI_ALLOW_ONLY_DEVELOPER_TOOL_CONFIGURATION_REGIONS": "false",
	} {
		if err := os.Setenv(k, v); err != nil {
			panic(err)
		}
	}
	for _, k := range []string{ociConfigFileEnv, "OCI_CLI_PROFILE", "OCI_CLI_AUTH"} {
		if err := os.Unsetenv(k); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}

const (
	testTenancy = "ocid1.tenancy.oc1..root"
	testRegion  = "us-ashburn-1"
)

var (
	testKeyOnce sync.Once
	testKeyPEM  string
)

// testPrivateKeyPEM returns a generated RSA key (PKCS#1 PEM) for signing.
func testPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	testKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	})
	return testKeyPEM
}

// fakeOCI is an in-memory OCI identity + container engine API.
type fakeOCI struct {
	// compartmentPages are the pages ListCompartments returns, in order.
	compartmentPages [][]map[string]any
	// compartmentsErr makes ListCompartments fail with this status.
	compartmentsErr int
	// clusterPages maps a compartment OCID to its ListClusters pages.
	clusterPages map[string][][]map[string]any
	// deniedCompartments get a 404 NotAuthorizedOrNotFound from ListClusters.
	deniedCompartments map[string]bool
	// failingCompartments make ListClusters fail with the given status.
	failingCompartments map[string]int
	// compartmentNames answers GetCompartment.
	compartmentNames map[string]string
	// kubeconfig is the CreateKubeconfig body.
	kubeconfig string

	mu              sync.Mutex
	kubeconfigBody  map[string]any
	listedClusters  []string
	authHeaderSeen  bool
	compartmentPage []string
}

func ociError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code, "message": code + " (fake)"})
}

func pageIndex(r *http.Request) int {
	var n int
	if p := r.URL.Query().Get("page"); p != "" {
		if _, err := fmt.Sscanf(p, "p%d", &n); err != nil {
			return 0
		}
	}
	return n
}

func writePage(w http.ResponseWriter, pages [][]map[string]any, idx int) {
	w.Header().Set("Content-Type", "application/json")
	if idx+1 < len(pages) {
		w.Header().Set("opc-next-page", fmt.Sprintf("p%d", idx+1))
	}
	items := []map[string]any{}
	if idx < len(pages) {
		items = pages[idx]
	}
	_ = json.NewEncoder(w).Encode(items)
}

func (f *fakeOCI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if strings.HasPrefix(r.Header.Get("Authorization"), "Signature ") {
		f.authHeaderSeen = true
	}
	f.mu.Unlock()

	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/20160918/compartments":
		if f.compartmentsErr != 0 {
			ociError(w, f.compartmentsErr, "NotAuthorizedOrNotFound")
			return
		}
		q := r.URL.Query()
		if q.Get("compartmentId") != testTenancy || q.Get("compartmentIdInSubtree") != "true" ||
			q.Get("accessLevel") != "ANY" || q.Get("lifecycleState") != "ACTIVE" {
			ociError(w, http.StatusBadRequest, "InvalidParameter")
			return
		}
		f.mu.Lock()
		f.compartmentPage = append(f.compartmentPage, q.Get("page"))
		f.mu.Unlock()
		writePage(w, f.compartmentPages, pageIndex(r))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/20160918/compartments/"):
		id := strings.TrimPrefix(path, "/20160918/compartments/")
		name, ok := f.compartmentNames[id]
		if !ok {
			ociError(w, http.StatusNotFound, "NotAuthorizedOrNotFound")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": name, "lifecycleState": "ACTIVE"})
	case r.Method == http.MethodGet && path == "/20180222/clusters":
		comp := r.URL.Query().Get("compartmentId")
		f.mu.Lock()
		f.listedClusters = append(f.listedClusters, comp+"#"+r.URL.Query().Get("page"))
		f.mu.Unlock()
		if f.deniedCompartments[comp] {
			ociError(w, http.StatusNotFound, "NotAuthorizedOrNotFound")
			return
		}
		if status := f.failingCompartments[comp]; status != 0 {
			ociError(w, status, "InternalServerError")
			return
		}
		writePage(w, f.clusterPages[comp], pageIndex(r))
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/20180222/clusters/") && strings.HasSuffix(path, "/kubeconfig/content"):
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		f.mu.Lock()
		f.kubeconfigBody = parsed
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, f.kubeconfig)
	default:
		ociError(w, http.StatusNotFound, "NotFound")
	}
}

// fakeOKEClients builds SDK clients signed with a generated key and pointed
// at the fake server.
func fakeOKEClients(t *testing.T, f *fakeOCI) (containerengine.ContainerEngineClient, identity.IdentityClient) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return fakeOKEClientsAt(t, srv.URL)
}

// fakeOKEClientsAt builds SDK clients signed with a generated key and
// pointed at url.
func fakeOKEClientsAt(t *testing.T, url string) (containerengine.ContainerEngineClient, identity.IdentityClient) {
	t.Helper()
	cp := common.NewRawConfigurationProvider(testTenancy, "ocid1.user.oc1..u", testRegion,
		"aa:bb:cc", testPrivateKeyPEM(t), nil)
	ce, err := containerengine.NewContainerEngineClientWithConfigurationProvider(cp)
	if err != nil {
		t.Fatal(err)
	}
	idc, err := identity.NewIdentityClientWithConfigurationProvider(cp)
	if err != nil {
		t.Fatal(err)
	}
	noRetry := common.NoRetryPolicy()
	ce.SetCustomClientConfiguration(common.CustomClientConfiguration{RetryPolicy: &noRetry})
	idc.SetCustomClientConfiguration(common.CustomClientConfiguration{RetryPolicy: &noRetry})
	ce.Host, idc.Host = url, url
	return ce, idc
}

func fakeOKEProvider(t *testing.T, f *fakeOCI, cfg OKEConfig) *OKEProvider {
	t.Helper()
	ce, idc := fakeOKEClients(t, f)
	region := cfg.Region
	if region == "" {
		region = testRegion
	}
	p, err := newOKEProviderWithClients(cfg, ce, idc, testTenancy, region)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func cluster(id, name, state, version string) map[string]any {
	m := map[string]any{"id": id, "name": name, "lifecycleState": state}
	if version != "" {
		m["kubernetesVersion"] = version
	}
	return m
}

func compartment(id, name string) map[string]any {
	return map[string]any{"id": id, "name": name, "lifecycleState": "ACTIVE"}
}

func newTenancyFake() *fakeOCI {
	return &fakeOCI{
		compartmentPages: [][]map[string]any{
			{compartment("ocid1.compartment.b", "beta"), compartment("ocid1.compartment.a", "alpha")},
			{compartment("ocid1.compartment.denied", "gamma")},
		},
		clusterPages: map[string][][]map[string]any{
			testTenancy: {{cluster("ocid1.cluster.zeta", "zeta", "ACTIVE", "v1.31.1")}},
			"ocid1.compartment.b": {
				{cluster("ocid1.cluster.mid", "mid", "ACTIVE", "v1.30.1")},
				{
					cluster("ocid1.cluster.gone", "gone", "DELETED", "v1.29.1"),
					cluster("ocid1.cluster.going", "going", "DELETING", "v1.29.1"),
					cluster("ocid1.cluster.new", "brand-new", "CREATING", "v1.31.1"),
				},
			},
			"ocid1.compartment.a": {{cluster("ocid1.cluster.aaa", "aaa", "ACTIVE", "")}},
		},
		deniedCompartments: map[string]bool{"ocid1.compartment.denied": true},
	}
}

func TestOKEListClusters_Tenancy(t *testing.T) {
	f := newTenancyFake()
	p := fakeOKEProvider(t, f, OKEConfig{ProfileID: "p1", ProfileName: "oracle"})

	clusters, err := p.ListClusters()
	if err != nil {
		t.Fatalf("ListClusters: %v", err)
	}
	var names []string
	for _, c := range clusters {
		names = append(names, c.Name)
	}
	if want := []string{"aaa", "brand-new", "mid", "zeta"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v (sorted, deleted/deleting skipped)", names, want)
	}

	byName := map[string]ClusterInfo{}
	for _, c := range clusters {
		byName[c.Name] = c
	}
	want := ClusterInfo{
		ID: "ocid1.cluster.mid", Name: "mid", State: "ACTIVE", Provider: "oke",
		ProfileID: "p1", ProfileName: "oracle", Region: testRegion,
		Description: "compartment: beta, kubernetes v1.30.1",
	}
	if got := byName["mid"]; got != want {
		t.Errorf("mid = %+v\nwant %+v", got, want)
	}
	if d := byName["zeta"].Description; d != "compartment: tenancy root, kubernetes v1.31.1" {
		t.Errorf("zeta description = %q", d)
	}
	if d := byName["aaa"].Description; d != "compartment: alpha" {
		t.Errorf("aaa description = %q", d)
	}
	if s := byName["brand-new"].State; s != "CREATING" {
		t.Errorf("brand-new state = %q", s)
	}

	// Both pages of compartments and of clusters in compartment b were read.
	if !reflect.DeepEqual(f.compartmentPage, []string{"", "p1"}) {
		t.Errorf("compartment pages requested = %v", f.compartmentPage)
	}
	listed := slices.Clone(f.listedClusters)
	sort.Strings(listed)
	wantListed := []string{"ocid1.compartment.a#", "ocid1.compartment.b#", "ocid1.compartment.b#p1", "ocid1.compartment.denied#", testTenancy + "#"}
	if !reflect.DeepEqual(listed, wantListed) {
		t.Errorf("cluster listings = %v, want %v", listed, wantListed)
	}
	if !f.authHeaderSeen {
		t.Error("requests were not signed")
	}
}

func TestOKEListClusters_SingleCompartmentAndRegion(t *testing.T) {
	f := newTenancyFake()
	f.compartmentNames = map[string]string{"ocid1.compartment.b": "beta"}
	p := fakeOKEProvider(t, f, OKEConfig{Compartment: "ocid1.compartment.b", Region: "eu-frankfurt-1"})

	clusters, err := p.ListClusters()
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 || clusters[0].Name != "brand-new" || clusters[1].Name != "mid" {
		t.Fatalf("clusters = %+v", clusters)
	}
	for _, c := range clusters {
		if c.Region != "eu-frankfurt-1" || !strings.HasPrefix(c.Description, "compartment: beta") {
			t.Errorf("cluster = %+v", c)
		}
	}
	if len(f.compartmentPage) != 0 {
		t.Errorf("compartments must not be enumerated with a compartment set: %v", f.compartmentPage)
	}
}

func TestOKEListClusters_UnknownCompartmentNameFallsBackToOCID(t *testing.T) {
	f := newTenancyFake()
	p := fakeOKEProvider(t, f, OKEConfig{Compartment: "ocid1.compartment.a"})
	clusters, err := p.ListClusters()
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 || clusters[0].Description != "compartment: ocid1.compartment.a" {
		t.Errorf("clusters = %+v", clusters)
	}
}

func TestOKEListClusters_AllListingsFail(t *testing.T) {
	f := newTenancyFake()
	f.deniedCompartments = map[string]bool{
		testTenancy: true, "ocid1.compartment.a": true, "ocid1.compartment.b": true, "ocid1.compartment.denied": true,
	}
	p := fakeOKEProvider(t, f, OKEConfig{})
	if _, err := p.ListClusters(); err == nil || !strings.Contains(err.Error(), "any compartment") {
		t.Errorf("error = %v, want failure when every listing fails", err)
	}
}

func TestOKEListClustersWithWarnings_PartialFailure(t *testing.T) {
	f := newTenancyFake() // ocid1.compartment.denied answers 404
	f.failingCompartments = map[string]int{"ocid1.compartment.b": http.StatusInternalServerError}
	p := fakeOKEProvider(t, f, OKEConfig{})

	clusters, warnings, err := p.ListClustersWithWarnings()
	if err != nil {
		t.Fatalf("err = %v, want nil on partial failure", err)
	}
	var names []string
	for _, c := range clusters {
		names = append(names, c.Name)
	}
	if want := []string{"aaa", "zeta"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].Error(), "ocid1.compartment.b") {
		t.Fatalf("warnings = %v, want exactly the 500 for compartment b", warnings)
	}
	var se common.ServiceError
	if !errors.As(warnings[0], &se) || se.GetHTTPStatusCode() != http.StatusInternalServerError {
		t.Errorf("warning does not wrap the 500 service error: %v", warnings[0])
	}

	// ListClusters keeps the ClusterProvider semantics.
	plain, err := p.ListClusters()
	if err != nil || len(plain) != 2 {
		t.Errorf("ListClusters = %v, %v", plain, err)
	}
}

func TestOKEListClustersWithWarnings_NoWarningsFor404(t *testing.T) {
	p := fakeOKEProvider(t, newTenancyFake(), OKEConfig{})
	clusters, warnings, err := p.ListClustersWithWarnings()
	if err != nil || len(warnings) != 0 || len(clusters) != 4 {
		t.Errorf("clusters = %d, warnings = %v, err = %v", len(clusters), warnings, err)
	}
}

func TestOKEListClusters_CompartmentEnumerationFails(t *testing.T) {
	f := newTenancyFake()
	f.compartmentsErr = http.StatusNotFound
	p := fakeOKEProvider(t, f, OKEConfig{})
	if _, err := p.ListClusters(); err == nil || !strings.Contains(err.Error(), "compartments") {
		t.Errorf("error = %v, want compartment enumeration error", err)
	}
}

func TestOKEListClusters_Empty(t *testing.T) {
	f := &fakeOCI{}
	p := fakeOKEProvider(t, f, OKEConfig{})
	clusters, err := p.ListClusters()
	if err != nil || len(clusters) != 0 {
		t.Errorf("clusters = %v, err = %v", clusters, err)
	}
}

func ociKubeconfig(clusterID string) string {
	ca := base64.StdEncoding.EncodeToString([]byte(testCAPEM))
	return `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: ` + ca + `
    server: https://10.0.0.1:6443
  name: cluster-c1
contexts:
- context:
    cluster: cluster-c1
    user: user-c1
  name: context-c1
current-context: context-c1
kind: ""
users:
- name: user-c1
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      args:
      - ce
      - cluster
      - generate-token
      - --cluster-id
      - ` + clusterID + `
      - --region
      - us-ashburn-1
      command: oci
      env: []
`
}

func execOf(t *testing.T, kubeconfig string) *clientcmdapi.ExecConfig {
	t.Helper()
	cfg, err := clientcmd.Load([]byte(kubeconfig))
	if err != nil {
		t.Fatalf("kubeconfig does not parse: %v\n%s", err, kubeconfig)
	}
	if err := clientcmd.Validate(*cfg); err != nil {
		t.Fatalf("kubeconfig invalid: %v", err)
	}
	if cfg.CurrentContext != "context-c1" {
		t.Errorf("current-context = %q", cfg.CurrentContext)
	}
	ai := cfg.AuthInfos["user-c1"]
	if ai == nil || ai.Exec == nil {
		t.Fatalf("exec user missing: %+v", cfg.AuthInfos)
	}
	return ai.Exec
}

func TestOKEGetKubeconfig_PinsArgs(t *testing.T) {
	const id = "ocid1.cluster.oc1.iad.c1"
	base := []string{"ce", "cluster", "generate-token", "--cluster-id", id, "--region", "us-ashburn-1"}

	tests := []struct {
		name         string
		cfg          OKEConfig
		wantArgs     []string
		wantEndpoint string
	}{
		{
			name:         "api key default profile",
			cfg:          OKEConfig{},
			wantArgs:     append(slices.Clone(base), "--profile", "DEFAULT", "--auth", "api_key"),
			wantEndpoint: "PUBLIC_ENDPOINT",
		},
		{
			name:         "api key named profile private endpoint",
			cfg:          OKEConfig{OCIProfile: "work", Auth: "api_key", Endpoint: "private"},
			wantArgs:     append(slices.Clone(base), "--profile", "work", "--auth", "api_key"),
			wantEndpoint: "PRIVATE_ENDPOINT",
		},
		{
			name:         "security token with config file",
			cfg:          OKEConfig{OCIProfile: "sess", Auth: "security_token", ConfigFile: "/etc/oci/config", Endpoint: "vcn_hostname"},
			wantArgs:     append(slices.Clone(base), "--profile", "sess", "--auth", "security_token", "--config-file", "/etc/oci/config"),
			wantEndpoint: "VCN_HOSTNAME",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeOCI{kubeconfig: ociKubeconfig(id)}
			p := fakeOKEProvider(t, f, tt.cfg)
			out, err := p.GetKubeconfig(id)
			if err != nil {
				t.Fatal(err)
			}
			exec := execOf(t, out)
			if exec.Command != "oci" || !reflect.DeepEqual(exec.Args, tt.wantArgs) {
				t.Errorf("exec = %s %v\nwant oci %v", exec.Command, exec.Args, tt.wantArgs)
			}
			if exec.InteractiveMode != clientcmdapi.NeverExecInteractiveMode {
				t.Errorf("interactiveMode = %q", exec.InteractiveMode)
			}
			if f.kubeconfigBody["tokenVersion"] != "2.0.0" || f.kubeconfigBody["endpoint"] != tt.wantEndpoint {
				t.Errorf("request body = %v, want tokenVersion 2.0.0 endpoint %s", f.kubeconfigBody, tt.wantEndpoint)
			}
			if p.EmbedsCredentials() {
				t.Error("EmbedsCredentials() = true")
			}
		})
	}
}

func TestOKEPinExecArgs_KeepsExistingFlags(t *testing.T) {
	p, err := newOKEProviderWithClients(OKEConfig{OCIProfile: "x", Auth: "security_token", ConfigFile: "/c"},
		containerengine.ContainerEngineClient{}, identity.IdentityClient{}, testTenancy, testRegion)
	if err != nil {
		t.Fatal(err)
	}
	in := []string{"ce", "--profile", "already", "--auth=security_token", "--config-file", "/other"}
	if got := p.pinExecArgs(in); !reflect.DeepEqual(got, in) {
		t.Errorf("args = %v, want unchanged %v", got, in)
	}
}

func TestOKEGetKubeconfig_EmptyOrInvalidBody(t *testing.T) {
	for name, body := range map[string]string{
		"empty":       "",
		"whitespace":  "  \n",
		"unparsable":  "{{{ not yaml",
		"no clusters": "apiVersion: v1\nkind: Config\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := fakeOKEProvider(t, &fakeOCI{kubeconfig: body}, OKEConfig{})
			if _, err := p.GetKubeconfig("ocid1.cluster.x"); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestNewOKEProvider_RejectsBadValues(t *testing.T) {
	path := writeOCIConfig(t, t.TempDir())
	for name, cfg := range map[string]OKEConfig{
		"auth":            {ConfigFile: path, Auth: "password"},
		"endpoint":        {ConfigFile: path, Endpoint: "elsewhere"},
		"missing profile": {ConfigFile: path, OCIProfile: "nope"},
		"missing file":    {ConfigFile: filepath.Join(t.TempDir(), "none")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewOKEProvider(cfg); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// writeOCIConfig writes an OCI config with DEFAULT and "other" profiles and
// a generated key file into dir.
func writeOCIConfig(t *testing.T, dir string) string {
	t.Helper()
	keyPath := writeFile(t, dir, "key.pem", testPrivateKeyPEM(t))
	return writeFile(t, dir, "config", fmt.Sprintf(`[DEFAULT]
user=ocid1.user.oc1..default
fingerprint=11:22:33
key_file=%[1]s
tenancy=%[2]s
region=us-phoenix-1

[other]
user=ocid1.user.oc1..other
fingerprint=44:55:66
key_file=%[1]s
tenancy=%[2]s
region=eu-frankfurt-1
`, keyPath, testTenancy))
}

func TestNewOKEProvider_RegionResolution(t *testing.T) {
	path := writeOCIConfig(t, t.TempDir())

	tests := []struct {
		cfg        OKEConfig
		wantRegion string
	}{
		{OKEConfig{ConfigFile: path}, "us-phoenix-1"},
		{OKEConfig{ConfigFile: path, OCIProfile: "other"}, "eu-frankfurt-1"},
		{OKEConfig{ConfigFile: path, OCIProfile: "other", Region: "ap-tokyo-1"}, "ap-tokyo-1"},
	}
	for _, tt := range tests {
		p, err := NewOKEProvider(tt.cfg)
		if err != nil {
			t.Fatalf("%+v: %v", tt.cfg, err)
		}
		if p.Region() != tt.wantRegion || p.tenancy != testTenancy || p.Type() != "oke" {
			t.Errorf("%+v: region=%q tenancy=%q", tt.cfg, p.Region(), p.tenancy)
		}
		for _, host := range []string{p.ce.Host, p.idc.Host} {
			if !strings.Contains(host, "."+tt.wantRegion+".") {
				t.Errorf("%+v: host %q does not target %s", tt.cfg, host, tt.wantRegion)
			}
		}
		_ = p.Close()
	}
}

func TestNewOKEProvider_SignsWithConfigFileKey(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeOCIConfig(t, dir)
	t.Setenv(ociConfigFileEnv, cfgPath)

	p, err := NewOKEProvider(OKEConfig{OCIProfile: "other"}) // path from env
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	f := newTenancyFake()
	srv := httptest.NewServer(f)
	defer srv.Close()
	p.ce.Host, p.idc.Host = srv.URL, srv.URL
	noRetry := common.NoRetryPolicy()
	p.ce.SetCustomClientConfiguration(common.CustomClientConfiguration{RetryPolicy: &noRetry})
	p.idc.SetCustomClientConfiguration(common.CustomClientConfiguration{RetryPolicy: &noRetry})

	clusters, err := p.ListClusters()
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 4 || clusters[0].Region != "eu-frankfurt-1" || !f.authHeaderSeen {
		t.Errorf("clusters = %+v signed = %v", clusters, f.authHeaderSeen)
	}
	// The env-selected config file is pinned, so kubectl uses the same file
	// even without OCI_CLI_CONFIG_FILE in its environment.
	want := []string{"ce", "--profile", "other", "--auth", "api_key", "--config-file", cfgPath}
	if args := p.pinExecArgs([]string{"ce"}); !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestNewOKEProvider_PinsRelativeConfigFileAsAbsolute(t *testing.T) {
	dir := t.TempDir()
	writeOCIConfig(t, dir)
	t.Chdir(dir)

	p, err := NewOKEProvider(OKEConfig{ConfigFile: "config"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	abs, err := filepath.Abs("config")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(p.configPath) || p.configPath != abs {
		t.Fatalf("configPath = %q, want %q", p.configPath, abs)
	}
	want := []string{"ce", "--profile", "DEFAULT", "--auth", "api_key", "--config-file", abs}
	if args := p.pinExecArgs([]string{"ce"}); !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestNewOKEProvider_DefaultConfigFileNotPinned(t *testing.T) {
	p, err := newOKEProviderWithClients(OKEConfig{OCIProfile: "x"},
		containerengine.ContainerEngineClient{}, identity.IdentityClient{}, testTenancy, testRegion)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ce", "--profile", "x", "--auth", "api_key"}
	if args := p.pinExecArgs([]string{"ce"}); !reflect.DeepEqual(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestResolveOCIConfigPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		"":              filepath.Join(home, ".oci", "config"),
		"~/x/config":    filepath.Join(home, "x", "config"),
		"rel/config":    filepath.Join(wd, "rel", "config"),
		"/abs/../c/cfg": "/c/cfg",
	} {
		if got := resolveOCIConfigPath(in); got != want {
			t.Errorf("resolveOCIConfigPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOKEClose_DropsIdleConnections(t *testing.T) {
	var (
		mu   sync.Mutex
		open = map[string]bool{}
	)
	srv := httptest.NewUnstartedServer(newTenancyFake())
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch s {
		case http.StateNew, http.StateActive, http.StateIdle:
			open[c.RemoteAddr().String()] = true
		case http.StateClosed, http.StateHijacked:
			delete(open, c.RemoteAddr().String())
		}
	}
	srv.Start()
	defer srv.Close()
	openConns := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(open)
	}
	waitFor := func(cond func(int) bool) int {
		deadline := time.Now().Add(5 * time.Second)
		for {
			n := openConns()
			if cond(n) || time.Now().After(deadline) {
				return n
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	ce, idc := fakeOKEClientsAt(t, srv.URL)
	p, err := newOKEProviderWithClients(OKEConfig{Compartment: "ocid1.compartment.a"}, ce, idc, testTenancy, testRegion)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListClusters(); err != nil {
		t.Fatal(err)
	}
	if n := waitFor(func(n int) bool { return n > 0 }); n == 0 {
		t.Fatal("expected a kept-alive connection after ListClusters")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if n := waitFor(func(n int) bool { return n == 0 }); n != 0 {
		t.Errorf("open connections after Close = %d, want 0", n)
	}
}

func TestListOCIProfiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ociConfigFileEnv, writeFile(t, dir, "config", `[DEFAULT]
user=ocid1.user.oc1..a
key_file=/keys/a#1.pem
tenancy=ocid1.tenancy.oc1..t

[zeta]
region=us-ashburn-1

[Alpha]
region=eu-frankfurt-1
`))
	got, err := ListOCIProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Alpha", "DEFAULT", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("profiles = %v, want %v", got, want)
	}
}

func TestListOCIProfiles_NoDefault(t *testing.T) {
	t.Setenv(ociConfigFileEnv, writeFile(t, t.TempDir(), "config", "[only]\nregion=us-ashburn-1\n"))
	got, err := ListOCIProfiles()
	if err != nil || !reflect.DeepEqual(got, []string{"only"}) {
		t.Errorf("profiles = %v, err = %v", got, err)
	}
}

func TestListOCIProfiles_MissingFile(t *testing.T) {
	t.Setenv(ociConfigFileEnv, filepath.Join(t.TempDir(), "missing"))
	got, err := ListOCIProfiles()
	if err != nil || got != nil {
		t.Errorf("profiles = %v, err = %v, want nil, nil", got, err)
	}
}

func TestListOCIRegions_SortedUnique(t *testing.T) {
	regions := ListOCIRegions()
	if !sort.StringsAreSorted(regions) || len(slices.Compact(slices.Clone(regions))) != len(regions) {
		t.Errorf("regions not sorted/unique: %v", regions)
	}
	for _, r := range []string{"us-ashburn-1", "us-phoenix-1", "eu-frankfurt-1", "ap-tokyo-1", "uk-london-1"} {
		if !slices.Contains(regions, r) {
			t.Errorf("missing %s", r)
		}
	}
}
