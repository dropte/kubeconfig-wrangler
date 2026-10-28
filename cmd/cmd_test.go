package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/kubeconfig-wrangler/pkg/provider"
)

var rancherEnvVars = []string{
	"RANCHER_URL", "RANCHER_ACCESS_KEY", "RANCHER_SECRET_KEY", "RANCHER_TOKEN",
	"RANCHER_USERNAME", "RANCHER_PASSWORD", "RANCHER_CLUSTER_PREFIX",
	"RANCHER_KUBECONFIG_OUTPUT", "RANCHER_INSECURE_SKIP_TLS_VERIFY", "RANCHER_CA_CERT",
}

func parseRancherFlags(t *testing.T, env map[string]string, args ...string) *cobra.Command {
	t.Helper()
	for _, k := range rancherEnvVars {
		t.Setenv(k, env[k])
	}
	c := &cobra.Command{Use: "test"}
	registerRancherFlags(c)
	if err := c.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRancherFlags_PasswordFlagsBeatEnvToken(t *testing.T) {
	c := parseRancherFlags(t, map[string]string{
		"RANCHER_URL":        "https://r.example.com",
		"RANCHER_TOKEN":      "env-ak:env-sk",
		"RANCHER_ACCESS_KEY": "env-ak2",
		"RANCHER_SECRET_KEY": "env-sk2",
	}, "--username", "other", "--password", "pw")

	cfg := rancherConfigFromFlags(c)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if !cfg.UsePasswordAuth() || cfg.Username != "other" || cfg.Password != "pw" {
		t.Errorf("expected password auth as other, got method=%q user=%q", cfg.AuthMethod, cfg.Username)
	}
}

func TestRancherFlags_KeyFlagsBeatEnvTokenAndPassword(t *testing.T) {
	c := parseRancherFlags(t, map[string]string{
		"RANCHER_URL":      "https://r.example.com",
		"RANCHER_TOKEN":    "env-ak:env-sk",
		"RANCHER_USERNAME": "envuser",
		"RANCHER_PASSWORD": "envpw",
	}, "--access-key", "flag-ak", "--secret-key", "flag-sk")

	cfg := rancherConfigFromFlags(c)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.UsePasswordAuth() || cfg.AccessKey != "flag-ak" || cfg.SecretKey != "flag-sk" {
		t.Errorf("expected flag key pair, got method=%q ak=%q", cfg.AuthMethod, cfg.AccessKey)
	}
	if cfg.Username != "" || cfg.Password != "" {
		t.Error("env username/password should be cleared by explicit key flags")
	}
}

func TestRancherFlags_TokenFlagBeatsEnvPassword(t *testing.T) {
	c := parseRancherFlags(t, map[string]string{
		"RANCHER_URL":      "https://r.example.com",
		"RANCHER_USERNAME": "envuser",
		"RANCHER_PASSWORD": "envpw",
	}, "--token", "ak:sk", "-k")

	cfg := rancherConfigFromFlags(c)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.UsePasswordAuth() || cfg.AccessKey != "ak" || !cfg.InsecureSkipTLSVerify {
		t.Errorf("unexpected config: method=%q ak=%q insecure=%v", cfg.AuthMethod, cfg.AccessKey, cfg.InsecureSkipTLSVerify)
	}
}

func TestRancherFlags_EnvOnlyUnchanged(t *testing.T) {
	c := parseRancherFlags(t, map[string]string{
		"RANCHER_URL":      "https://r.example.com/",
		"RANCHER_TOKEN":    "ak:sk",
		"RANCHER_USERNAME": "envuser",
		"RANCHER_PASSWORD": "envpw",
	})

	cfg := rancherConfigFromFlags(c)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.UsePasswordAuth() || cfg.RancherURL != "https://r.example.com" {
		t.Errorf("env-only: method=%q url=%q", cfg.AuthMethod, cfg.RancherURL)
	}
}

func envFunc(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveEKSCredentials(t *testing.T) {
	ambient := map[string]string{
		"AWS_ACCESS_KEY_ID":     "ENVAK",
		"AWS_SECRET_ACCESS_KEY": "ENVSK",
		"AWS_SESSION_TOKEN":     "ENVST",
		"AWS_REGION":            "us-east-2",
	}

	t.Run("flag keys ignore env session token", func(t *testing.T) {
		cfg, err := resolveEKSCredentials(eksFlagValues{accessKey: "AK", secretKey: "SK"}, envFunc(ambient))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AccessKey != "AK" || cfg.SecretKey != "SK" || cfg.SessionToken != "" || cfg.AWSProfile != "" {
			t.Errorf("got %+v", cfg)
		}
		if cfg.Region != "us-east-2" {
			t.Errorf("region = %q", cfg.Region)
		}
	})

	t.Run("profile flag ignores env keys", func(t *testing.T) {
		cfg, err := resolveEKSCredentials(eksFlagValues{profile: "prod", region: "eu-west-1"}, envFunc(ambient))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AWSProfile != "prod" || cfg.AccessKey != "" || cfg.SecretKey != "" || cfg.SessionToken != "" {
			t.Errorf("got %+v", cfg)
		}
		if cfg.Region != "eu-west-1" {
			t.Errorf("region = %q", cfg.Region)
		}
	})

	t.Run("AWS_PROFILE env ignores env keys", func(t *testing.T) {
		env := map[string]string{"AWS_PROFILE": "envprof"}
		for k, v := range ambient {
			env[k] = v
		}
		cfg, err := resolveEKSCredentials(eksFlagValues{}, envFunc(env))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AWSProfile != "envprof" || cfg.AccessKey != "" {
			t.Errorf("got %+v", cfg)
		}
	})

	t.Run("env keys with session token", func(t *testing.T) {
		cfg, err := resolveEKSCredentials(eksFlagValues{}, envFunc(ambient))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AccessKey != "ENVAK" || cfg.SecretKey != "ENVSK" || cfg.SessionToken != "ENVST" {
			t.Errorf("got %+v", cfg)
		}
	})

	for name, f := range map[string]eksFlagValues{
		"profile with keys":        {profile: "p", accessKey: "AK", secretKey: "SK"},
		"only access key":          {accessKey: "AK"},
		"session token alone":      {sessionToken: "ST"},
		"profile with secret only": {profile: "p", secretKey: "SK"},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if _, err := resolveEKSCredentials(f, envFunc(nil)); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func isolateAWSEnv(t *testing.T, configContent string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	if err := os.WriteFile(configPath, []byte(configContent), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", configPath)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, k := range []string{"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_PROFILE", "AWS_DEFAULT_PROFILE",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		t.Setenv(k, "")
	}
}

func TestResolveEKSRegion_FromProfile(t *testing.T) {
	isolateAWSEnv(t, "[default]\nregion = us-east-1\n\n[profile prod]\nregion = ap-southeast-2\n")

	cfg, err := resolveEKSCredentials(eksFlagValues{profile: "prod"}, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolveEKSRegion(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Region != "ap-southeast-2" {
		t.Errorf("region = %q, want ap-southeast-2", cfg.Region)
	}
}

func TestResolveEKSRegion_FlagWins(t *testing.T) {
	isolateAWSEnv(t, "[profile prod]\nregion = ap-southeast-2\n")

	cfg, err := resolveEKSCredentials(eksFlagValues{profile: "prod", region: "us-west-2"}, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolveEKSRegion(&cfg); err != nil || cfg.Region != "us-west-2" {
		t.Errorf("region = %q, err = %v", cfg.Region, err)
	}
}

func TestResolveEKSRegion_Missing(t *testing.T) {
	isolateAWSEnv(t, "[profile prod]\noutput = json\n")

	cfg, err := resolveEKSCredentials(eksFlagValues{profile: "prod"}, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolveEKSRegion(&cfg); err == nil {
		t.Error("expected an error when no region can be resolved")
	}
}

func TestWriteOutputFile_TightensPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeOutputFile(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("mode = %o, want 600", perm)
	}
	if data, _ := os.ReadFile(path); string(data) != "new" {
		t.Errorf("content = %q", data)
	}
}

func TestCountErrors(t *testing.T) {
	if n := countErrors(nil); n != 0 {
		t.Errorf("nil: %d", n)
	}
	if n := countErrors(errors.New("x")); n != 1 {
		t.Errorf("single: %d", n)
	}
	if n := countErrors(errors.Join(errors.New("a"), errors.New("b"), errors.New("c"))); n != 3 {
		t.Errorf("joined: %d", n)
	}
}

func TestResolveServeToken(t *testing.T) {
	t.Setenv(serveTokenEnv, "from-env")
	if tok, fromEnv, _ := resolveServeToken("from-flag"); tok != "from-flag" || fromEnv {
		t.Errorf("flag should win, got %q fromEnv=%v", tok, fromEnv)
	}
	if v, ok := os.LookupEnv(serveTokenEnv); ok {
		t.Errorf("env token must be unset, still %q", v)
	}

	t.Setenv(serveTokenEnv, "from-env")
	if tok, fromEnv, _ := resolveServeToken(""); tok != "from-env" || !fromEnv {
		t.Errorf("env token expected, got %q fromEnv=%v", tok, fromEnv)
	}
	if _, ok := os.LookupEnv(serveTokenEnv); ok {
		t.Error("env token must be unset after use")
	}

	a, fromEnv, err := resolveServeToken("")
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := resolveServeToken("")
	if len(a) != 64 || a == b || fromEnv {
		t.Errorf("random tokens should be 64 hex chars and differ: %q %q (fromEnv=%v)", a, b, fromEnv)
	}
}

func TestServeOpenMessage_RedactsEnvToken(t *testing.T) {
	const tok = "deadbeefcafe"
	if msg := serveOpenMessage("127.0.0.1:8080", tok, true); strings.Contains(msg, tok) || strings.Contains(msg, "token=") {
		t.Errorf("env token leaked in %q", msg)
	}
	if msg := serveOpenMessage("127.0.0.1:8080", tok, false); msg != "Open http://127.0.0.1:8080/?token="+tok {
		t.Errorf("generated/flag token message = %q", msg)
	}
}

func TestGenerateFlags_EmptyFlagsOverrideEnv(t *testing.T) {
	c := parseRancherFlags(t, map[string]string{
		"RANCHER_KUBECONFIG_OUTPUT": "/tmp/from-env",
		"RANCHER_CLUSTER_PREFIX":    "env-",
		"RANCHER_CA_CERT":           "/env/ca.pem",
		"RANCHER_URL":               "https://env.example.com",
	})
	var prefix, output string
	c.Flags().StringVarP(&prefix, "prefix", "p", "", "")
	c.Flags().StringVarP(&output, "output", "o", "", "")
	if err := c.ParseFlags([]string{"-o", "", "-p", "", "--ca-cert", ""}); err != nil {
		t.Fatal(err)
	}

	cfg := rancherConfigFromFlags(c)
	applyGenerateFlags(c, cfg)
	if cfg.OutputPath != "" || cfg.ClusterPrefix != "" || cfg.CACert != "" {
		t.Errorf("output=%q prefix=%q ca=%q, want all empty", cfg.OutputPath, cfg.ClusterPrefix, cfg.CACert)
	}
	if cfg.RancherURL != "https://env.example.com" {
		t.Errorf("unchanged flag must keep env value, url=%q", cfg.RancherURL)
	}
}

func TestGenerateFlags_UnsetFlagsKeepEnv(t *testing.T) {
	c := parseRancherFlags(t, map[string]string{
		"RANCHER_KUBECONFIG_OUTPUT": "/tmp/from-env",
		"RANCHER_CLUSTER_PREFIX":    "env-",
	})
	c.Flags().StringP("prefix", "p", "", "")
	c.Flags().StringP("output", "o", "", "")
	cfg := rancherConfigFromFlags(c)
	applyGenerateFlags(c, cfg)
	if cfg.OutputPath != "/tmp/from-env" || cfg.ClusterPrefix != "env-" {
		t.Errorf("output=%q prefix=%q, want env values", cfg.OutputPath, cfg.ClusterPrefix)
	}
}

func TestRootCommandSilencesUsageAndErrors(t *testing.T) {
	if !rootCmd.SilenceUsage || !rootCmd.SilenceErrors {
		t.Error("rootCmd must set SilenceUsage and SilenceErrors")
	}
}

// parseOKEFlags parses args against a fresh command carrying the oke
// persistent flags, as a subcommand would see them.
func parseOKEFlags(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: "test"}
	c.Flags().AddFlagSet(okeCmd.PersistentFlags())
	// AddFlagSet shares *pflag.Flag values; reset them so tests do not leak.
	c.Flags().VisitAll(func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	})
	if err := c.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Flags().VisitAll(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	})
	return c
}

func TestOKEConfigFromFlags(t *testing.T) {
	env := envFunc(map[string]string{"OCI_CLI_PROFILE": "envprof", "OCI_CLI_CONFIG_FILE": "/env/config"})

	t.Run("env used when flags absent", func(t *testing.T) {
		cfg, err := okeConfigFromFlags(parseOKEFlags(t), env)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.OCIProfile != "envprof" || cfg.ConfigFile != "/env/config" || cfg.Region != "" || cfg.Compartment != "" {
			t.Errorf("got %+v", cfg)
		}
	})

	t.Run("explicit flags win", func(t *testing.T) {
		c := parseOKEFlags(t, "--profile", "flagprof", "--config-file", "/flag/config", "-r", "eu-frankfurt-1",
			"--compartment", "ocid1.compartment.x", "--auth", "security_token", "--endpoint", "private")
		cfg, err := okeConfigFromFlags(c, env)
		if err != nil {
			t.Fatal(err)
		}
		want := provider.OKEConfig{
			ProfileID: "cli", ProfileName: "CLI", OCIProfile: "flagprof", ConfigFile: "/flag/config",
			Region: "eu-frankfurt-1", Compartment: "ocid1.compartment.x", Auth: "security_token", Endpoint: "private",
		}
		if cfg != want {
			t.Errorf("got %+v\nwant %+v", cfg, want)
		}
	})

	t.Run("explicit empty flags clear env", func(t *testing.T) {
		cfg, err := okeConfigFromFlags(parseOKEFlags(t, "--profile", "", "--config-file", ""), env)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.OCIProfile != "" || cfg.ConfigFile != "" {
			t.Errorf("got %+v", cfg)
		}
	})

	for name, args := range map[string][]string{
		"bad auth":     {"--auth", "password"},
		"bad endpoint": {"--endpoint", "elsewhere"},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if _, err := okeConfigFromFlags(parseOKEFlags(t, args...), envFunc(nil)); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestOKECommandsRegistered(t *testing.T) {
	want := map[string]bool{"list": false, "generate": false, "profiles": false, "compartments": false, "regions": false}
	for _, c := range okeCmd.Commands() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("oke %s not registered", name)
		}
	}
	found := false
	for _, c := range rootCmd.Commands() {
		found = found || c == okeCmd
	}
	if !found {
		t.Error("oke command not registered on root")
	}
	for _, name := range []string{"prefix", "output"} {
		if okeGenerateCmd.Flags().Lookup(name) == nil {
			t.Errorf("oke generate --%s missing", name)
		}
	}
}

func TestOKEKubeconfigKeyAndCompartment(t *testing.T) {
	taken := map[string]string{"prod": "x"}
	if k := okeKubeconfigKey(provider.ClusterInfo{Name: "dev", ID: "ocid1.cluster.aaaaaa111111"}, taken); k != "dev" {
		t.Errorf("unique key = %q", k)
	}
	if k := okeKubeconfigKey(provider.ClusterInfo{Name: "prod", ID: "ocid1.cluster.aaaaaa111111"}, taken); k != "prod-111111" {
		t.Errorf("duplicate key = %q", k)
	}
	taken["prod-111111"] = "y"
	if k := okeKubeconfigKey(provider.ClusterInfo{Name: "prod", ID: "ocid1.cluster.bbbbbb111111"}, taken); k != "prod-111111-2" {
		t.Errorf("second duplicate key = %q", k)
	}

	for in, want := range map[string]string{
		"compartment: team, kubernetes v1.31.1": "team",
		"compartment: tenancy root":             "tenancy root",
	} {
		if got := okeCompartmentOf(in); got != want {
			t.Errorf("okeCompartmentOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// writeOCIConfig writes a scratch OCI config with DEFAULT and alpha profiles.
func writeOCIConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oci-config")
	content := "[DEFAULT]\nregion=us-ashburn-1\n\n[alpha]\nregion=eu-frankfurt-1\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runOKEProfilesFor(t *testing.T, args ...string) string {
	t.Helper()
	c := parseOKEFlags(t, args...)
	var out bytes.Buffer
	c.SetOut(&out)
	if err := runOKEProfiles(c, nil); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestOKEProfiles_ConfigFileFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OCI_CLI_CONFIG_FILE", filepath.Join(t.TempDir(), "missing"))
	cfg := writeOCIConfig(t)

	out := runOKEProfilesFor(t, "--config-file", cfg)
	if !strings.Contains(out, "  DEFAULT\n") || !strings.Contains(out, "  alpha\n") || !strings.Contains(out, cfg) {
		t.Errorf("--config-file profiles not listed:\n%s", out)
	}
	if got := os.Getenv("OCI_CLI_CONFIG_FILE"); !strings.HasSuffix(got, "missing") {
		t.Errorf("OCI_CLI_CONFIG_FILE not restored, got %q", got)
	}
}

func TestOKEProfiles_EnvAndDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := writeOCIConfig(t)

	t.Setenv("OCI_CLI_CONFIG_FILE", cfg)
	if out := runOKEProfilesFor(t); !strings.Contains(out, "  alpha\n") {
		t.Errorf("env config file not used:\n%s", out)
	}

	// An explicit empty flag clears the env value: ~/.oci/config (absent).
	out := runOKEProfilesFor(t, "--config-file", "")
	if !strings.Contains(out, "No OCI profiles found in ~/.oci/config") {
		t.Errorf("expected empty default listing, got:\n%s", out)
	}
	if os.Getenv("OCI_CLI_CONFIG_FILE") != cfg {
		t.Error("OCI_CLI_CONFIG_FILE not restored")
	}
}
