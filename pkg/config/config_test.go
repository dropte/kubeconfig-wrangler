package config

import (
	"testing"
)

func TestConfig_Validate_RequiresURL(t *testing.T) {
	cfg := &Config{}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error when RancherURL is empty")
	}
	if err.Error() != "rancher URL is required" {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestConfig_Validate_RequiresAuth(t *testing.T) {
	cfg := &Config{
		RancherURL: "https://rancher.example.com",
	}
	err := cfg.Validate()
	if err == nil {
		t.Error("expected error when no authentication is provided")
	}
	if err.Error() != "authentication required: provide either token/access_key+secret_key or username+password" {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestConfig_Validate_TokenAuth(t *testing.T) {
	tests := []struct {
		name        string
		config      *Config
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid token",
			config: &Config{
				RancherURL: "https://rancher.example.com",
				Token:      "token-xxxxx:secretkey",
			},
			expectError: false,
		},
		{
			name: "valid access_key and secret_key",
			config: &Config{
				RancherURL: "https://rancher.example.com",
				AccessKey:  "token-xxxxx",
				SecretKey:  "secretkey",
			},
			expectError: false,
		},
		{
			name: "invalid token format - no colon",
			config: &Config{
				RancherURL: "https://rancher.example.com",
				Token:      "invalidtoken",
			},
			expectError: true,
			errorMsg:    "invalid token format, expected 'access_key:secret_key'",
		},
		{
			name: "missing secret_key",
			config: &Config{
				RancherURL: "https://rancher.example.com",
				AccessKey:  "token-xxxxx",
			},
			expectError: true,
			errorMsg:    "authentication required: provide either token/access_key+secret_key or username+password",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.expectError {
				if err == nil {
					t.Error("expected error but got none")
				} else if err.Error() != tt.errorMsg {
					t.Errorf("expected error %q, got %q", tt.errorMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestConfig_Validate_PasswordAuth(t *testing.T) {
	tests := []struct {
		name        string
		config      *Config
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid username and password",
			config: &Config{
				RancherURL: "https://rancher.example.com",
				Username:   "admin",
				Password:   "password123",
			},
			expectError: false,
		},
		{
			name: "missing password",
			config: &Config{
				RancherURL: "https://rancher.example.com",
				Username:   "admin",
			},
			expectError: true,
			errorMsg:    "authentication required: provide either token/access_key+secret_key or username+password",
		},
		{
			name: "missing username",
			config: &Config{
				RancherURL: "https://rancher.example.com",
				Password:   "password123",
			},
			expectError: true,
			errorMsg:    "authentication required: provide either token/access_key+secret_key or username+password",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.expectError {
				if err == nil {
					t.Error("expected error but got none")
				} else if err.Error() != tt.errorMsg {
					t.Errorf("expected error %q, got %q", tt.errorMsg, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestConfig_Validate_TokenParsing(t *testing.T) {
	cfg := &Config{
		RancherURL: "https://rancher.example.com",
		Token:      "token-xxxxx:secretkey",
	}

	err := cfg.Validate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.AccessKey != "token-xxxxx" {
		t.Errorf("expected AccessKey to be 'token-xxxxx', got %q", cfg.AccessKey)
	}
	if cfg.SecretKey != "secretkey" {
		t.Errorf("expected SecretKey to be 'secretkey', got %q", cfg.SecretKey)
	}
	if cfg.AuthMethod != AuthMethodToken {
		t.Errorf("expected AuthMethod to be %q, got %q", AuthMethodToken, cfg.AuthMethod)
	}
}

func TestConfig_Validate_PasswordAuthMethod(t *testing.T) {
	cfg := &Config{
		RancherURL: "https://rancher.example.com",
		Username:   "admin",
		Password:   "password123",
	}

	err := cfg.Validate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.AuthMethod != AuthMethodPassword {
		t.Errorf("expected AuthMethod to be %q, got %q", AuthMethodPassword, cfg.AuthMethod)
	}
}

func TestConfig_Validate_TokenPreferredOverPassword(t *testing.T) {
	cfg := &Config{
		RancherURL: "https://rancher.example.com",
		Token:      "token-xxxxx:secretkey",
		Username:   "admin",
		Password:   "password123",
	}

	err := cfg.Validate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// When both are provided, token should be preferred
	if cfg.AuthMethod != AuthMethodToken {
		t.Errorf("expected AuthMethod to be %q when both are provided, got %q", AuthMethodToken, cfg.AuthMethod)
	}
}

func TestConfig_Validate_TrimsTrailingSlash(t *testing.T) {
	cfg := &Config{
		RancherURL: "https://rancher.example.com/",
		Token:      "token-xxxxx:secretkey",
	}

	err := cfg.Validate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.RancherURL != "https://rancher.example.com" {
		t.Errorf("expected trailing slash to be trimmed, got %q", cfg.RancherURL)
	}
}

func TestConfig_UsePasswordAuth(t *testing.T) {
	tests := []struct {
		name       string
		authMethod AuthMethod
		expected   bool
	}{
		{
			name:       "password auth",
			authMethod: AuthMethodPassword,
			expected:   true,
		},
		{
			name:       "token auth",
			authMethod: AuthMethodToken,
			expected:   false,
		},
		{
			name:       "empty auth method",
			authMethod: "",
			expected:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{AuthMethod: tt.authMethod}
			if got := cfg.UsePasswordAuth(); got != tt.expected {
				t.Errorf("UsePasswordAuth() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestConfig_GetBasicAuth(t *testing.T) {
	cfg := &Config{
		AccessKey: "myaccesskey",
		SecretKey: "mysecretkey",
	}

	username, password := cfg.GetBasicAuth()

	if username != "myaccesskey" {
		t.Errorf("expected username to be 'myaccesskey', got %q", username)
	}
	if password != "mysecretkey" {
		t.Errorf("expected password to be 'mysecretkey', got %q", password)
	}
}

func TestLoadFromEnv(t *testing.T) {
	// Set environment variables
	envVars := map[string]string{
		"RANCHER_URL":                      "https://rancher.example.com",
		"RANCHER_ACCESS_KEY":               "access123",
		"RANCHER_SECRET_KEY":               "secret456",
		"RANCHER_TOKEN":                    "token-xxx:yyy",
		"RANCHER_USERNAME":                 "testuser",
		"RANCHER_PASSWORD":                 "testpass",
		"RANCHER_CLUSTER_PREFIX":           "prod-",
		"RANCHER_KUBECONFIG_OUTPUT":        "/tmp/kubeconfig",
		"RANCHER_INSECURE_SKIP_TLS_VERIFY": "true",
		"RANCHER_CA_CERT":                  "/path/to/ca.crt",
	}

	// t.Setenv restores the original values when the test ends.
	for key, value := range envVars {
		t.Setenv(key, value)
	}

	cfg := LoadFromEnv()

	if cfg.RancherURL != "https://rancher.example.com" {
		t.Errorf("RancherURL = %q, want %q", cfg.RancherURL, "https://rancher.example.com")
	}
	if cfg.AccessKey != "access123" {
		t.Errorf("AccessKey = %q, want %q", cfg.AccessKey, "access123")
	}
	if cfg.SecretKey != "secret456" {
		t.Errorf("SecretKey = %q, want %q", cfg.SecretKey, "secret456")
	}
	if cfg.Token != "token-xxx:yyy" {
		t.Errorf("Token = %q, want %q", cfg.Token, "token-xxx:yyy")
	}
	if cfg.Username != "testuser" {
		t.Errorf("Username = %q, want %q", cfg.Username, "testuser")
	}
	if cfg.Password != "testpass" {
		t.Errorf("Password = %q, want %q", cfg.Password, "testpass")
	}
	if cfg.ClusterPrefix != "prod-" {
		t.Errorf("ClusterPrefix = %q, want %q", cfg.ClusterPrefix, "prod-")
	}
	if cfg.OutputPath != "/tmp/kubeconfig" {
		t.Errorf("OutputPath = %q, want %q", cfg.OutputPath, "/tmp/kubeconfig")
	}
	if !cfg.InsecureSkipTLSVerify {
		t.Error("InsecureSkipTLSVerify should be true")
	}
	if cfg.CACert != "/path/to/ca.crt" {
		t.Errorf("CACert = %q, want %q", cfg.CACert, "/path/to/ca.crt")
	}
}

func TestLoadFromEnv_InsecureSkipTLSVerify_False(t *testing.T) {
	t.Setenv("RANCHER_INSECURE_SKIP_TLS_VERIFY", "false")
	cfg := LoadFromEnv()
	if cfg.InsecureSkipTLSVerify {
		t.Error("InsecureSkipTLSVerify should be false when set to 'false'")
	}

	t.Setenv("RANCHER_INSECURE_SKIP_TLS_VERIFY", "")
	cfg = LoadFromEnv()
	if cfg.InsecureSkipTLSVerify {
		t.Error("InsecureSkipTLSVerify should be false when empty")
	}
}

func TestParseBoolEnv(t *testing.T) {
	cases := map[string]bool{
		"true": true, "TRUE": true, "True": true, "1": true, "t": true, "T": true,
		"yes": true, "YES": true, " true ": true,
		"false": false, "0": false, "no": false, "": false, "maybe": false, "2": false,
	}
	for in, want := range cases {
		if got := ParseBoolEnv(in); got != want {
			t.Errorf("ParseBoolEnv(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestLoadFromEnv_InsecureSkipTLSVerify_Variants(t *testing.T) {
	for _, v := range []string{"1", "TRUE", "yes"} {
		t.Setenv("RANCHER_INSECURE_SKIP_TLS_VERIFY", v)
		if !LoadFromEnv().InsecureSkipTLSVerify {
			t.Errorf("RANCHER_INSECURE_SKIP_TLS_VERIFY=%q should enable skip-verify", v)
		}
	}
}
