package web

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// renderIndex returns the page served to a token holder.
func renderIndex(t *testing.T) string {
	t.Helper()
	e := setup(t)
	rec := e.do(http.MethodGet, "/?token="+testToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func indexScript(t *testing.T, page string) string {
	t.Helper()
	start := strings.Index(page, "<script>")
	end := strings.LastIndex(page, "</script>")
	if start < 0 || end < start {
		t.Fatal("page has no <script> block")
	}
	return page[start+len("<script>") : end]
}

func TestIndexElementIDs(t *testing.T) {
	page := renderIndex(t)
	script := indexScript(t, page)

	ids := map[string]int{}
	for _, m := range regexp.MustCompile(`\bid="([^"$]+)"`).FindAllStringSubmatch(page, -1) {
		ids[m[1]]++
	}
	for id, n := range ids {
		if n > 1 {
			t.Errorf("id %q is used %d times", id, n)
		}
	}
	for _, m := range regexp.MustCompile(`getElementById\('([^']+)'\)`).FindAllStringSubmatch(script, -1) {
		if ids[m[1]] == 0 {
			t.Errorf("script references missing element #%s", m[1])
		}
	}
}

func TestIndexScriptContract(t *testing.T) {
	page := renderIndex(t)
	script := indexScript(t, page)

	// Dynamic strings must not be interpolated into inline handlers (F010).
	if m := regexp.MustCompile(`on[a-z]+="[^"]*'\$\{`).FindString(script); m != "" {
		t.Errorf("inline handler interpolates a string: %s", m)
	}
	// The kubeconfig path comes from the server, not a hard-coded default.
	if strings.Contains(page, "~/.kube/config") {
		t.Error("page hard-codes ~/.kube/config")
	}
	for _, want := range []string{
		"/api/status",
		"mergeToKubeconfig: options.mergeToKubeconfig",
		"X-Kubeconfig-Warnings",
		"openKubeconfigFile",
		"(unchanged)",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script does not contain %q", want)
		}
	}
	// Merge now goes through the generate endpoints; the old client-side
	// context-name tracking must be gone (F028).
	if strings.Contains(script, "/api/context/merge") {
		t.Error("script still posts to /api/context/merge")
	}
	// Token and keys are trimmed before sending; the password is not (R019).
	for _, want := range []string{
		"profileData.token = token.trim()",
		"profileData.secretKey = secretKey.trim()",
		"profileData.sessionToken = sessionToken.trim()",
		"profileData.password = password;",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script does not contain %q", want)
		}
	}
	// Secrets are never prefilled (F006).
	if m := regexp.MustCompile(`profile\.(token|password|secretKey|sessionToken|kubeconfig)\b`).FindString(script); m != "" {
		t.Errorf("script reads %s", m)
	}
}

// TestIndexScriptSyntax runs node --check on the served script when node is
// available, catching broken template-literal splicing in the Go constant.
func TestIndexScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := indexScript(t, renderIndex(t))
	path := filepath.Join(t.TempDir(), "index.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
		t.Fatalf("node --check failed: %v\n%s", err, out)
	}
}
