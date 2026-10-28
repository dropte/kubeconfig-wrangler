package provider

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const testCAPEM = "-----BEGIN CERTIFICATE-----\nMIIBfake\n-----END CERTIFICATE-----\n"

func loadGenerated(t *testing.T, p *EKSProvider, name string) *clientcmdapi.Config {
	t.Helper()
	out, err := p.generateKubeconfig(name, "https://ABC.gr7.us-west-2.eks.amazonaws.com",
		base64.StdEncoding.EncodeToString([]byte(testCAPEM)))
	if err != nil {
		t.Fatalf("generateKubeconfig: %v", err)
	}
	cfg, err := clientcmd.Load([]byte(out))
	if err != nil {
		t.Fatalf("generated kubeconfig does not parse: %v\n%s", err, out)
	}
	if err := clientcmd.Validate(*cfg); err != nil {
		t.Fatalf("generated kubeconfig is invalid: %v", err)
	}
	return cfg
}

func envMap(env []clientcmdapi.ExecEnvVar) map[string]string {
	m := make(map[string]string)
	for _, e := range env {
		m[e.Name] = e.Value
	}
	return m
}

func TestEKSGenerateKubeconfig_Profile(t *testing.T) {
	p := &EKSProvider{config: EKSConfig{Region: "us-west-2", AWSProfile: "prod"}}
	cfg := loadGenerated(t, p, "my-cluster")

	if cfg.CurrentContext != "my-cluster" {
		t.Errorf("current-context = %q", cfg.CurrentContext)
	}
	cluster := cfg.Clusters["my-cluster"]
	if cluster == nil {
		t.Fatal("cluster missing")
	}
	if cluster.Server != "https://ABC.gr7.us-west-2.eks.amazonaws.com" {
		t.Errorf("server = %q", cluster.Server)
	}
	if string(cluster.CertificateAuthorityData) != testCAPEM {
		t.Errorf("CA data = %q, want decoded PEM", cluster.CertificateAuthorityData)
	}

	exec := cfg.AuthInfos["my-cluster"].Exec
	if exec == nil {
		t.Fatal("exec config missing")
	}
	wantArgs := []string{"--profile", "prod", "eks", "get-token", "--cluster-name", "my-cluster", "--region", "us-west-2"}
	if exec.Command != "aws" || !reflect.DeepEqual(exec.Args, wantArgs) {
		t.Errorf("exec = %s %v, want aws %v", exec.Command, exec.Args, wantArgs)
	}
	if exec.APIVersion != "client.authentication.k8s.io/v1beta1" {
		t.Errorf("apiVersion = %q", exec.APIVersion)
	}
	if exec.InteractiveMode != clientcmdapi.NeverExecInteractiveMode {
		t.Errorf("interactiveMode = %q", exec.InteractiveMode)
	}
	if len(exec.Env) != 0 {
		t.Errorf("profile mode must not embed env, got %v", exec.Env)
	}
}

func TestEKSGenerateKubeconfig_DirectCredentials(t *testing.T) {
	p := &EKSProvider{config: EKSConfig{
		Region:       "eu-west-1",
		AccessKey:    "AKIAEXAMPLE",
		SecretKey:    "secret/with: special#chars",
		SessionToken: "session-token",
	}}
	if !p.EmbedsCredentials() {
		t.Error("EmbedsCredentials() = false")
	}
	cfg := loadGenerated(t, p, "c1")

	exec := cfg.AuthInfos["c1"].Exec
	wantArgs := []string{"eks", "get-token", "--cluster-name", "c1", "--region", "eu-west-1"}
	if !reflect.DeepEqual(exec.Args, wantArgs) {
		t.Errorf("args = %v, want %v", exec.Args, wantArgs)
	}
	want := map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIAEXAMPLE",
		"AWS_SECRET_ACCESS_KEY": "secret/with: special#chars",
		"AWS_SESSION_TOKEN":     "session-token",
	}
	if got := envMap(exec.Env); !reflect.DeepEqual(got, want) {
		t.Errorf("env = %v, want %v", got, want)
	}
}

func TestEKSGenerateKubeconfig_ProfileAndCredentialsWithoutSessionToken(t *testing.T) {
	p := &EKSProvider{config: EKSConfig{Region: "us-east-1", AWSProfile: "dev", AccessKey: "AK", SecretKey: "SK"}}
	cfg := loadGenerated(t, p, "c2")

	// Static keys take precedence (as in NewEKSProvider), so --profile must
	// not be passed: it would make the aws CLI ignore the env credentials.
	exec := cfg.AuthInfos["c2"].Exec
	wantArgs := []string{"eks", "get-token", "--cluster-name", "c2", "--region", "us-east-1"}
	if !reflect.DeepEqual(exec.Args, wantArgs) {
		t.Errorf("args = %v, want %v", exec.Args, wantArgs)
	}
	want := map[string]string{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK"}
	if got := envMap(exec.Env); !reflect.DeepEqual(got, want) {
		t.Errorf("env = %v, want %v", got, want)
	}
}

func TestEKSGenerateKubeconfig_SpecialClusterName(t *testing.T) {
	name := "weird: name #1"
	p := &EKSProvider{config: EKSConfig{Region: "us-east-1"}}
	cfg := loadGenerated(t, p, name)

	if cfg.CurrentContext != name {
		t.Errorf("current-context = %q, want %q", cfg.CurrentContext, name)
	}
	if _, ok := cfg.Clusters[name]; !ok {
		t.Errorf("cluster %q missing: %v", name, cfg.Clusters)
	}
	if args := cfg.AuthInfos[name].Exec.Args; args[3] != name {
		t.Errorf("cluster-name arg = %q, want %q", args[3], name)
	}
}

func TestEKSGenerateKubeconfig_InvalidCA(t *testing.T) {
	p := &EKSProvider{config: EKSConfig{Region: "us-east-1"}}
	if _, err := p.generateKubeconfig("c", "https://x", "not base64!!"); err == nil {
		t.Error("expected error for invalid CA data")
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestListAWSProfiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", writeFile(t, dir, "credentials", `
[zeta]
aws_access_key_id = a
aws_secret_access_key = b

[alpha]
aws_access_key_id = a
`))
	t.Setenv("AWS_CONFIG_FILE", writeFile(t, dir, "config", `
[default]
region = us-east-1

[profile  spaced]
region = us-west-2

[profile alpha]
region = eu-west-1
s3 =
  max_concurrent_requests = 10

[sso-session corp]
sso_region = us-east-1
`))

	got, err := ListAWSProfiles()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "default", "spaced", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("profiles = %v, want %v", got, want)
	}
}

func TestListAWSProfiles_NoPhantomDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", writeFile(t, dir, "credentials", "[work]\naws_access_key_id = a\n"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "missing-config"))

	got, err := ListAWSProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"work"}; !reflect.DeepEqual(got, want) {
		t.Errorf("profiles = %v, want %v", got, want)
	}
}

func TestListAWSProfiles_NoFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "none1"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "none2"))

	got, err := ListAWSProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("profiles = %v, want none", got)
	}
}
