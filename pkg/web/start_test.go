package web

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

// TestStartBindFailureReturnsError: a port that is already taken makes Start
// fail and never logs readiness (R002).
func TestStartBindFailureReturnsError(t *testing.T) {
	logs := captureLog(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	e := setup(t)
	e.srv.addr = ln.Addr().String()
	errc := make(chan error, 1)
	go func() { errc <- e.srv.Start() }()
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "listen on") {
			t.Fatalf("Start = %v, want listen error", err)
		}
	case <-time.After(5 * time.Second):
		_ = e.srv.Shutdown(context.Background())
		t.Fatal("Start did not fail on a bound port")
	}
	if strings.Contains(logs.String(), "Listening on") || strings.Contains(logs.String(), "Starting web server") {
		t.Fatalf("readiness logged for a failed bind:\n%s", logs.String())
	}
}

// TestStartLogsListeningAfterBind: "Listening on" is logged with the bound
// address, and the server answers there by the time it is logged.
func TestStartLogsListeningAfterBind(t *testing.T) {
	logs := captureLog(t)
	e := setup(t)
	e.srv.addr = "127.0.0.1:0"
	errc := make(chan error, 1)
	go func() { errc <- e.srv.Start() }()

	re := regexp.MustCompile(`Listening on (127\.0\.0\.1:\d+)`)
	var addr string
	deadline := time.Now().Add(5 * time.Second)
	for addr == "" && time.Now().Before(deadline) {
		if m := re.FindStringSubmatch(logs.String()); m != nil {
			addr = m[1]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		t.Fatalf("no Listening line:\n%s", logs.String())
	}
	// The listener is bound, so a connection succeeds immediately.
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	_ = conn.Close()

	if err := e.srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("Start after Shutdown = %v, want nil", err)
	}
}

func TestStartRequiresToken(t *testing.T) {
	e := setup(t)
	e.srv.token = ""
	if err := e.srv.Start(); err == nil {
		t.Fatal("Start without a token succeeded")
	}
}

// TestAllowedHosts covers wildcard, empty-host and port-80 binds (R008).
func TestAllowedHosts(t *testing.T) {
	hostname, _ := os.Hostname()
	hostname = strings.ToLower(hostname)

	cases := []struct {
		addr       string
		allowed    []string
		disallowed []string
	}{
		{
			addr:       "127.0.0.1:8080",
			allowed:    []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"},
			disallowed: []string{"127.0.0.1", "localhost", "evil.example:8080", "127.0.0.1:9090"},
		},
		{
			addr:       "0.0.0.0:8080",
			allowed:    []string{"0.0.0.0:8080", "127.0.0.1:8080", "localhost:8080"},
			disallowed: []string{"evil.example:8080", "localhost"},
		},
		{
			addr:       ":8080",
			allowed:    []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"},
			disallowed: []string{":8080", "evil.example:8080"},
		},
		{
			addr:       "[::]:8080",
			allowed:    []string{"[::]:8080", "127.0.0.1:8080", "[::1]:8080"},
			disallowed: []string{"evil.example:8080"},
		},
		{
			addr:       "127.0.0.1:80",
			allowed:    []string{"127.0.0.1:80", "127.0.0.1", "localhost", "[::1]"},
			disallowed: []string{"evil.example", "evil.example:80"},
		},
	}
	for _, c := range cases {
		hosts := allowedHosts(c.addr)
		for _, h := range c.allowed {
			if !hosts[h] {
				t.Errorf("allowedHosts(%q): %q not allowed", c.addr, h)
			}
		}
		for _, h := range c.disallowed {
			if hosts[h] {
				t.Errorf("allowedHosts(%q): %q allowed", c.addr, h)
			}
		}
	}

	// Wildcard binds accept the machine's hostname and interface addresses.
	for _, addr := range []string{"0.0.0.0:8080", ":8080"} {
		hosts := allowedHosts(addr)
		if hostname != "" && !hosts[hostname+":8080"] {
			t.Errorf("allowedHosts(%q) lacks hostname %q", addr, hostname)
		}
		for _, n := range localNames() {
			if want := strings.ToLower(net.JoinHostPort(n, "8080")); !hosts[want] {
				t.Errorf("allowedHosts(%q) lacks %q", addr, want)
			}
		}
	}
	// A specific loopback bind does not open up the LAN names.
	if hostname != "" && hostname != "localhost" && allowedHosts("127.0.0.1:8080")[hostname+":8080"] {
		t.Error("loopback bind accepts the hostname")
	}
}

func TestAllowHostsAndEnv(t *testing.T) {
	t.Setenv(AllowedHostsEnv, " proxy.example , localhost:9000,")
	e := setup(t)
	e.srv.AllowHosts("Wrangler.LAN", "[fd00::1]:7000")

	for _, host := range []string{
		"proxy.example", "proxy.example:18080", "localhost:9000",
		"wrangler.lan", "wrangler.lan:18080", "[fd00::1]:7000",
	} {
		rec := e.do("GET", "/api/status", "", "Host", host, "X-Auth-Token", testToken)
		if rec.Code != http.StatusOK {
			t.Errorf("Host %q: got %d, want 200", host, rec.Code)
		}
	}
	for _, host := range []string{"proxy.example:1", "localhost:9001", "[fd00::1]:18080", "evil.example"} {
		rec := e.do("GET", "/api/status", "", "Host", host, "X-Auth-Token", testToken)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Host %q: got %d, want 403", host, rec.Code)
		}
	}
}

func TestOriginHTTPSBehindProxy(t *testing.T) {
	e := setup(t)
	e.srv.AllowHosts("wrangler.example")
	rec := e.do("POST", "/api/cluster/alias", `{}`, "Host", "wrangler.example",
		"X-Auth-Token", testToken, "Content-Type", "application/json", "Origin", "https://wrangler.example")
	if rec.Code == http.StatusForbidden {
		t.Fatalf("https origin of an allowed host was rejected: %s", rec.Body.String())
	}
	rec = e.do("POST", "/api/cluster/alias", `{}`, "Host", "wrangler.example",
		"X-Auth-Token", testToken, "Content-Type", "application/json", "Origin", "https://evil.example")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign origin: got %d, want 403", rec.Code)
	}
}

func TestBrowserURL(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8080": "http://127.0.0.1:8080/?token=t%2Bk",
		"0.0.0.0:8080":   "http://127.0.0.1:8080/?token=t%2Bk",
		":8080":          "http://127.0.0.1:8080/?token=t%2Bk",
		"[::]:8080":      "http://127.0.0.1:8080/?token=t%2Bk",
		"[::1]:8080":     "http://[::1]:8080/?token=t%2Bk",
		"10.0.0.5:3000":  "http://10.0.0.5:3000/?token=t%2Bk",
	}
	for addr, want := range cases {
		if got := BrowserURL(addr, "t+k"); got != want {
			t.Errorf("BrowserURL(%q) = %q, want %q", addr, got, want)
		}
	}
}
