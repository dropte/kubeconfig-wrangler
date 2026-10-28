// Package web provides a web-based GUI for kubeconfig-wrangler
package web

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	kctx "github.com/kubeconfig-wrangler/pkg/context"
	"github.com/kubeconfig-wrangler/pkg/kubeconfig"
	"github.com/kubeconfig-wrangler/pkg/profile"
	"github.com/kubeconfig-wrangler/pkg/provider"
)

// Version is reported by /api/status. The cmd package sets it at startup.
var Version = "dev"

// warningsHeader carries the per-cluster failures of a partially successful
// download as a JSON array of "cluster: error" strings.
const warningsHeader = "X-Kubeconfig-Warnings"

// Server represents the web server
type Server struct {
	addr         string
	token        string
	mux          *http.ServeMux
	profileStore *profile.Store
	storeErr     error
	ctxSwitcher  *kctx.Switcher
	indexTmpl    *template.Template
	indexErr     error
	allowedHosts map[string]bool

	mu      sync.Mutex
	httpSrv *http.Server
}

// APIResponse represents a generic API response
type APIResponse struct {
	Success  bool        `json:"success"`
	Data     interface{} `json:"data,omitempty"`
	Error    string      `json:"error,omitempty"`
	Warnings []string    `json:"warnings,omitempty"`
}

// ClusterSelection represents a selected cluster with both ID and display name
type ClusterSelection struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Alias     string `json:"alias,omitempty"`
	ProfileID string `json:"profileId,omitempty"`
}

// DisplayName returns the alias if set, otherwise the name
func (c ClusterSelection) DisplayName() string {
	if c.Alias != "" {
		return c.Alias
	}
	return c.Name
}

// ClusterFailure describes a selected cluster whose kubeconfig could not be
// fetched.
type ClusterFailure struct {
	Cluster string `json:"cluster"`
	Error   string `json:"error"`
}

// MergeResult is the response data of a merge into the local kubeconfig.
type MergeResult struct {
	Added    []string         `json:"added"`
	Replaced []string         `json:"replaced"`
	Failed   []ClusterFailure `json:"failed"`
}

// NewServer creates a new web server. token must be non-empty; Start refuses
// to run without one.
func NewServer(addr string, token string) *Server {
	store, err := profile.NewStore()
	if err != nil {
		log.Printf("Warning: failed to initialize profile store: %v", err)
	}
	return newServer(addr, token, store, err, kctx.NewSwitcher())
}

func newServer(addr, token string, store *profile.Store, storeErr error, switcher *kctx.Switcher) *Server {
	s := &Server{
		addr:         addr,
		token:        token,
		mux:          http.NewServeMux(),
		profileStore: store,
		storeErr:     storeErr,
		ctxSwitcher:  switcher,
		allowedHosts: allowedHosts(addr),
	}
	s.AllowHosts(allowedHostsFromEnv()...)
	if store == nil && storeErr == nil {
		s.storeErr = errors.New("not initialized")
	}
	s.indexTmpl, s.indexErr = template.New("index").Parse(indexHTML)
	if s.indexErr != nil {
		log.Printf("Error parsing index template: %v", s.indexErr)
	}
	s.setupRoutes()
	return s
}

// AllowedHostsEnv names an environment variable holding extra Host header
// values (comma separated) that the server accepts. See AllowHosts.
const AllowedHostsEnv = "KUBECONFIG_WRANGLER_ALLOWED_HOSTS"

// allowedHosts returns the Host header values accepted for a server bound to
// addr: the loopback names for its port, plus addr's own host. For a wildcard bind (empty host, 0.0.0.0 or
// ::) the machine's hostname and interface addresses are accepted too, so LAN
// clients work. On port 80 the port-less forms are accepted as well, since
// browsers omit the default port. Anything else (e.g. a DNS-rebinding
// attacker's domain) is rejected; extra names can be added with AllowHosts.
func allowedHosts(addr string) map[string]bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return map[string]bool{strings.ToLower(addr): true}
	}
	hosts := map[string]bool{}
	names := []string{"127.0.0.1", "localhost", "::1"}
	if host != "" {
		names = append(names, host)
	}
	if isUnspecifiedHost(host) {
		names = append(names, localNames()...)
	}
	for _, n := range names {
		addHost(hosts, n, port, port == "80")
	}
	return hosts
}

// addHost adds name with port to hosts and, when bare is set, name without a
// port. IPv6 literals are bracketed as browsers send them.
func addHost(hosts map[string]bool, name, port string, bare bool) {
	name = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(name, "["), "]"))
	if name == "" {
		return
	}
	hosts[net.JoinHostPort(name, port)] = true
	if bare {
		if strings.Contains(name, ":") {
			hosts["["+name+"]"] = true
		} else {
			hosts[name] = true
		}
	}
}

// isUnspecifiedHost reports whether host is a wildcard bind address.
func isUnspecifiedHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// localNames returns this machine's hostname and interface addresses
// (link-local IPv6 addresses are skipped; browsers cannot use them without a
// zone).
func localNames() []string {
	var names []string
	if h, err := os.Hostname(); err == nil && h != "" {
		names = append(names, h)
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return names
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLinkLocalUnicast() {
			continue
		}
		names = append(names, ipNet.IP.String())
	}
	return names
}

// AllowHosts adds Host header values the server accepts, for access through a
// DNS name, a reverse proxy or a container port mapping. An entry with a port
// ("host:9000", "[::1]:9000") must match exactly; an entry without one
// ("example.com") matches with the server's port and with no port (as sent
// for a proxy's default port). Call it before Start.
func (s *Server) AllowHosts(entries ...string) {
	_, port, err := net.SplitHostPort(s.addr)
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if _, _, perr := net.SplitHostPort(e); perr == nil {
			s.allowedHosts[e] = true
			continue
		}
		if err != nil {
			s.allowedHosts[e] = true
			continue
		}
		addHost(s.allowedHosts, e, port, true)
	}
}

// allowedHostsFromEnv returns the entries of AllowedHostsEnv.
func allowedHostsFromEnv() []string {
	v := os.Getenv(AllowedHostsEnv)
	if v == "" {
		return nil
	}
	return strings.Split(v, ",")
}

// BrowserURL returns the URL a browser should open for a server bound to
// addr. A wildcard or empty host is replaced by 127.0.0.1 (http://0.0.0.0 is
// not openable in current browsers) and IPv6 hosts are bracketed.
func BrowserURL(addr, token string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr + "/?token=" + url.QueryEscape(token)
	}
	if isUnspecifiedHost(host) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/?token=" + url.QueryEscape(token)
}

// sortedAllowedHosts lists the accepted Host values, for logging.
func (s *Server) sortedAllowedHosts() []string {
	out := make([]string, 0, len(s.allowedHosts))
	for h := range s.allowedHosts {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// isLoopbackAddr reports whether addr's host is a loopback address or name.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// setupRoutes configures the HTTP routes
func (s *Server) setupRoutes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/status", s.handleStatus)

	// Profile management
	s.mux.HandleFunc("/api/profiles", s.handleProfiles)
	s.mux.HandleFunc("/api/profiles/", s.handleProfileByID)

	// Cluster operations with profiles
	s.mux.HandleFunc("/api/clusters/profile", s.handleListClustersForProfile)
	s.mux.HandleFunc("/api/clusters/all", s.handleListAllClusters)
	s.mux.HandleFunc("/api/generate/profile", s.handleGenerateForProfile)
	s.mux.HandleFunc("/api/generate/all", s.handleGenerateForAllSources)

	// AWS-specific endpoints
	s.mux.HandleFunc("/api/aws/profiles", s.handleAWSProfiles)
	s.mux.HandleFunc("/api/aws/regions", s.handleAWSRegions)

	// Oracle Cloud (OKE) endpoints
	s.mux.HandleFunc("/api/oci/profiles", s.handleOCIProfiles)
	s.mux.HandleFunc("/api/oci/regions", s.handleOCIRegions)

	// Context management
	s.mux.HandleFunc("/api/context", s.handleContext)
	s.mux.HandleFunc("/api/contexts", s.handleContexts)
	s.mux.HandleFunc("/api/context/delete", s.handleContextDelete)
	s.mux.HandleFunc("/api/context/rename", s.handleContextRename)

	// Cluster alias management
	s.mux.HandleFunc("/api/cluster/alias", s.handleClusterAlias)
}

// tokenMatches compares a presented token with the server token in constant
// time. An empty server token never matches.
func (s *Server) tokenMatches(presented string) bool {
	if s.token == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) == 1
}

// unauthorizedPage is served for "/" when the request carries no valid token.
const unauthorizedPage = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>kubeconfig-wrangler</title></head>
<body><h1>Unauthorized</h1>
<p>Open the URL printed when kubeconfig-wrangler started (it includes <code>?token=...</code>).</p>
</body></html>
`

// securityMiddleware guards every request: Host allowlist (DNS rebinding),
// token on "/" and /api/*, and CSRF checks on state-changing API calls.
func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedHosts[strings.ToLower(r.Host)] {
			log.Printf("Blocked request with unexpected Host header: %q", r.Host)
			http.Error(w, "Forbidden: invalid host", http.StatusForbidden)
			return
		}

		if r.URL.Path == "/" {
			presented := r.URL.Query().Get("token")
			if presented == "" {
				presented = r.Header.Get("X-Auth-Token")
			}
			if !s.tokenMatches(presented) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(unauthorizedPage))
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}

		if !s.tokenMatches(r.Header.Get("X-Auth-Token")) {
			log.Printf("Blocked API request with invalid or missing token")
			http.Error(w, "Unauthorized: invalid or missing token", http.StatusUnauthorized)
			return
		}

		// The Host is already allowlisted; https is accepted for a
		// TLS-terminating reverse proxy in front of the server.
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host && origin != "https://"+r.Host {
			log.Printf("Blocked request with invalid origin: %s", origin)
			http.Error(w, "Forbidden: invalid origin", http.StatusForbidden)
			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				log.Printf("Blocked %s request with Sec-Fetch-Site %q", r.Method, site)
				http.Error(w, "Forbidden: cross-site request", http.StatusForbidden)
				return
			}
			// A JSON content type cannot be sent cross-origin without a CORS
			// preflight (which this server never approves), so requiring it
			// stops simple-request CSRF. A body-less DELETE is exempt: DELETE
			// always needs a preflight anyway.
			bodyless := r.ContentLength == 0 && len(r.TransferEncoding) == 0
			if (r.Method != http.MethodDelete || !bodyless) && !isJSONContentType(r.Header.Get("Content-Type")) {
				http.Error(w, "Unsupported Media Type: application/json required", http.StatusUnsupportedMediaType)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func isJSONContentType(ct string) bool {
	mediaType, _, err := mime.ParseMediaType(ct)
	return err == nil && mediaType == "application/json"
}

// Handler returns the server's HTTP handler with the security middleware.
func (s *Server) Handler() http.Handler {
	return s.securityMiddleware(s.mux)
}

// Start binds addr and serves until the server stops. Readiness ("Listening
// on ...") is logged only after the port is bound, and a bind failure (for
// example EADDRINUSE) is returned instead of being reported as started.
func (s *Server) Start() error {
	if s.token == "" {
		return errors.New("refusing to start the web server without a security token")
	}
	if !isLoopbackAddr(s.addr) {
		log.Printf("WARNING: listening on non-loopback address %s; the GUI and all saved credentials are reachable from the network by anyone holding the token", s.addr)
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}
	return s.serve(ln)
}

// serve serves on an already bound listener.
func (s *Server) serve(ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.mu.Lock()
	s.httpSrv = srv
	s.mu.Unlock()
	log.Printf("Accepted Host headers: %s", strings.Join(s.sortedAllowedHosts(), ", "))
	log.Printf("Listening on %s", ln.Addr())
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops a server started with Start.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.httpSrv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// templateData holds data passed to the HTML template
type templateData struct {
	AuthToken string
}

// handleIndex serves the main HTML page. The middleware only lets requests
// carrying the token reach it, so embedding the token is safe.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if s.indexTmpl == nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := s.indexTmpl.Execute(&buf, templateData{AuthToken: s.token}); err != nil {
		log.Printf("Error executing template: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The URL carries the token: keep it out of caches and Referer headers.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if _, err := w.Write(buf.Bytes()); err != nil {
		log.Printf("Error writing index page: %v", err)
	}
}

// handleStatus reports the server's health for the UI.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}
	status := struct {
		StoreAvailable bool     `json:"storeAvailable"`
		StoreError     string   `json:"storeError,omitempty"`
		LoadWarnings   []string `json:"loadWarnings"`
		KubeconfigPath string   `json:"kubeconfigPath"`
		Version        string   `json:"version"`
	}{
		StoreAvailable: s.profileStore != nil,
		LoadWarnings:   []string{},
		KubeconfigPath: s.ctxSwitcher.Path(),
		Version:        Version,
	}
	if s.profileStore != nil {
		if warnings := s.profileStore.LoadWarnings(); len(warnings) > 0 {
			status.LoadWarnings = warnings
		}
	} else if s.storeErr != nil {
		status.StoreError = s.storeErr.Error()
	}
	s.writeJSON(w, http.StatusOK, APIResponse{Success: true, Data: status})
}

// storeAvailable writes a 500 and returns false when the profile store could
// not be initialised.
func (s *Server) storeAvailable(w http.ResponseWriter) bool {
	if s.profileStore != nil {
		return true
	}
	s.writeJSON(w, http.StatusInternalServerError, APIResponse{
		Success: false,
		Error:   fmt.Sprintf("profile store unavailable: %v", s.storeErr),
	})
	return false
}

func (s *Server) methodNotAllowed(w http.ResponseWriter) {
	s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
		Success: false,
		Error:   "Method not allowed",
	})
}

// writeJSON writes a JSON response
func (s *Server) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("Error encoding JSON response: %v", err)
	}
}

// setWarningsHeader adds the X-Kubeconfig-Warnings header listing failures.
// Non-ASCII characters are \u-escaped so the value is a valid header.
func setWarningsHeader(w http.ResponseWriter, failures []string) {
	if len(failures) == 0 {
		return
	}
	data, err := json.Marshal(failures)
	if err != nil {
		return
	}
	var b strings.Builder
	for _, r := range string(data) {
		if r < utf8.RuneSelf {
			b.WriteRune(r)
			continue
		}
		r1, r2 := utf16.EncodeRune(r)
		if r1 == unicode.ReplacementChar {
			fmt.Fprintf(&b, `\u%04x`, r)
		} else {
			fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
		}
	}
	w.Header().Set(warningsHeader, b.String())
}

// sanitizeSourceName converts a source/profile name to a safe prefix format
// e.g., "My Rancher Server" -> "my-rancher-server"
func sanitizeSourceName(name string) string {
	// Convert to lowercase
	result := strings.ToLower(name)
	// Replace spaces and underscores with dashes
	result = strings.ReplaceAll(result, " ", "-")
	result = strings.ReplaceAll(result, "_", "-")
	// Remove any characters that aren't alphanumeric or dashes
	var cleaned strings.Builder
	for _, r := range result {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			cleaned.WriteRune(r)
		}
	}
	result = cleaned.String()
	// Remove consecutive dashes
	for strings.Contains(result, "--") {
		result = strings.ReplaceAll(result, "--", "-")
	}
	// Trim leading/trailing dashes
	result = strings.Trim(result, "-")
	return result
}

// sourcePrefixFor returns the "<sanitized profile name>-" prefix, or "" when
// the name sanitises to nothing.
func sourcePrefixFor(profileName string) string {
	if name := sanitizeSourceName(profileName); name != "" {
		return name + "-"
	}
	return ""
}

// uniqueKey returns name, or name-2, name-3, ... whichever is not taken.
func uniqueKey(name string, taken func(string) bool) string {
	if !taken(name) {
		return name
	}
	for i := 2; ; i++ {
		candidate := name + "-" + strconv.Itoa(i)
		if !taken(candidate) {
			return candidate
		}
	}
}

// windowsReservedNames are file names Windows refuses regardless of extension.
var windowsReservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// sanitizeZipName turns a context name into a file name (without extension)
// that extracts on every OS: characters illegal on Windows (<>:"/\|?*) and
// control characters become "-", leading/trailing dots and spaces are
// trimmed, and reserved device names get a "-" suffix.
func sanitizeZipName(name string) string {
	mapped := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '-'
		}
		return r
	}, name)
	mapped = strings.Trim(mapped, ". ")
	if mapped == "" {
		return "cluster"
	}
	if windowsReservedNames[strings.ToUpper(mapped)] {
		mapped += "-"
	}
	return mapped
}

// handleProfiles handles profile listing and creation
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listProfiles(w, r)
	case http.MethodPost:
		s.createProfile(w, r)
	default:
		s.methodNotAllowed(w)
	}
}

func (s *Server) listProfiles(w http.ResponseWriter, r *http.Request) {
	if !s.storeAvailable(w) {
		return
	}

	profiles := s.profileStore.List()
	sort.Slice(profiles, func(i, j int) bool {
		if profiles[i].Name != profiles[j].Name {
			return profiles[i].Name < profiles[j].Name
		}
		return profiles[i].ID < profiles[j].ID
	})
	views := make([]profile.ProfileView, len(profiles))
	for i, p := range profiles {
		views[i] = p.View()
	}
	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    views,
	})
}

func (s *Server) createProfile(w http.ResponseWriter, r *http.Request) {
	if !s.storeAvailable(w) {
		return
	}

	var req profile.ProfileCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	trimSecretFields(&req)
	p, err := s.profileStore.Create(&req)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to create profile: %v", err),
		})
		return
	}

	s.writeJSON(w, http.StatusCreated, APIResponse{
		Success: true,
		Data:    p.View(),
	})
}

// trimSecretFields strips surrounding whitespace from the API token, AWS
// secret key and session token, which never legitimately contain it (a pasted
// trailing newline would otherwise cause 401s or broken SigV4 signatures).
// The password is left exactly as sent.
func trimSecretFields(req *profile.ProfileCreateRequest) {
	req.Token = strings.TrimSpace(req.Token)
	req.SecretKey = strings.TrimSpace(req.SecretKey)
	req.SessionToken = strings.TrimSpace(req.SessionToken)
}

// handleProfileByID handles operations on a specific profile
func (s *Server) handleProfileByID(w http.ResponseWriter, r *http.Request) {
	if !s.storeAvailable(w) {
		return
	}

	// Extract profile ID from path
	id := strings.TrimPrefix(r.URL.Path, "/api/profiles/")
	if id == "" {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "Profile ID required",
		})
		return
	}

	switch r.Method {
	case http.MethodGet:
		p, err := s.profileStore.Get(id)
		if err != nil {
			s.writeJSON(w, http.StatusNotFound, APIResponse{
				Success: false,
				Error:   err.Error(),
			})
			return
		}
		s.writeJSON(w, http.StatusOK, APIResponse{
			Success: true,
			Data:    p.View(),
		})

	case http.MethodPut:
		var req profile.ProfileCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("Invalid request: %v", err),
			})
			return
		}

		trimSecretFields(&req)
		p, err := s.profileStore.Update(id, &req)
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("Failed to update profile: %v", err),
			})
			return
		}
		s.writeJSON(w, http.StatusOK, APIResponse{
			Success: true,
			Data:    p.View(),
		})

	case http.MethodDelete:
		if err := s.profileStore.Delete(id); err != nil {
			s.writeJSON(w, http.StatusNotFound, APIResponse{
				Success: false,
				Error:   err.Error(),
			})
			return
		}
		s.writeJSON(w, http.StatusOK, APIResponse{
			Success: true,
		})

	default:
		s.methodNotAllowed(w)
	}
}

// markClustersInLocalKubeconfig checks which clusters exist in the local kubeconfig
// and sets the InLocalKubeconfig field accordingly
func (s *Server) markClustersInLocalKubeconfig(clusters []provider.ClusterInfo) {
	localNames, err := s.ctxSwitcher.GetLocalClusterNames()
	if err != nil {
		log.Printf("Warning: failed to get local cluster names: %v", err)
		return
	}

	for i := range clusters {
		// Check if the cluster name or alias exactly matches any local cluster/context name
		if localNames[clusters[i].Name] {
			clusters[i].InLocalKubeconfig = true
			continue
		}
		if clusters[i].Alias != "" && localNames[clusters[i].Alias] {
			clusters[i].InLocalKubeconfig = true
			continue
		}

		// Check if this cluster was exported (merged) with a tracked context name
		if s.profileStore != nil && clusters[i].ProfileID != "" {
			exportedContextName := s.profileStore.GetExportedContext(clusters[i].ProfileID, clusters[i].ID)
			if exportedContextName != "" && localNames[exportedContextName] {
				clusters[i].InLocalKubeconfig = true
			}
		}
	}
}

// applyAliases sets each cluster's Alias from the profile's stored aliases.
func applyAliases(clusters []provider.ClusterInfo, aliases map[string]string) {
	for i := range clusters {
		if alias, ok := aliases[clusters[i].ID]; ok {
			clusters[i].Alias = alias
		}
	}
}

// handleListClustersForProfile lists clusters for a specific profile
func (s *Server) handleListClustersForProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w)
		return
	}

	var req struct {
		ProfileID string `json:"profileId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	if !s.storeAvailable(w) {
		return
	}

	p, err := s.profileStore.Get(req.ProfileID)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	clusters, err := s.getClustersForProfile(p)
	if err != nil {
		log.Printf("Listing clusters for profile %s failed: %v", p.Name, err)
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to list clusters: %v", err),
		})
		return
	}

	applyAliases(clusters, p.ClusterAliases)
	sort.SliceStable(clusters, func(i, j int) bool { return clusters[i].Name < clusters[j].Name })
	s.markClustersInLocalKubeconfig(clusters)

	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    clusters,
	})
}

// newProviderForProfile builds the cluster provider for a profile. The caller
// must Close it.
func newProviderForProfile(p *profile.Profile) (provider.ClusterProvider, error) {
	if p.Locked {
		return nil, fmt.Errorf("profile %q could not be decrypted; edit it and re-enter its credentials", p.Name)
	}
	switch p.Type {
	case profile.ProfileTypeRancher:
		return provider.NewRancherProvider(provider.RancherConfig{
			ProfileID:     p.ID,
			ProfileName:   p.Name,
			URL:           p.RancherURL,
			Token:         p.Token,
			Username:      p.Username,
			Password:      p.Password,
			SkipTLSVerify: p.SkipTLS,
			CACert:        p.CACert,
		})
	case profile.ProfileTypeEKS:
		return provider.NewEKSProvider(provider.EKSConfig{
			ProfileID:    p.ID,
			ProfileName:  p.Name,
			Region:       p.AWSRegion,
			AWSProfile:   p.AWSProfile,
			AccessKey:    p.AccessKey,
			SecretKey:    p.SecretKey,
			SessionToken: p.SessionToken,
		})
	case profile.ProfileTypeOKE:
		return provider.NewOKEProvider(provider.OKEConfig{
			ProfileID:   p.ID,
			ProfileName: p.Name,
			OCIProfile:  p.OCIProfile,
			ConfigFile:  p.OCIConfigFile,
			Region:      p.OCIRegion,
			Compartment: p.OCICompartment,
			Auth:        p.OCIAuth,
			Endpoint:    p.OKEEndpoint,
		})
	case profile.ProfileTypeStatic:
		return provider.NewStaticProvider(provider.StaticConfig{
			ProfileID:   p.ID,
			ProfileName: p.Name,
			Kubeconfig:  p.Kubeconfig,
		})
	default:
		return nil, fmt.Errorf("unknown profile type: %s", p.Type)
	}
}

func (s *Server) getClustersForProfile(p *profile.Profile) ([]provider.ClusterInfo, error) {
	prov, err := newProviderForProfile(p)
	if err != nil {
		return nil, err
	}
	defer prov.Close()
	return prov.ListClusters()
}

// handleListAllClusters returns clusters from all profiles
func (s *Server) handleListAllClusters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}

	if !s.storeAvailable(w) {
		return
	}

	allProfiles := s.profileStore.List()

	var allClusters []provider.ClusterInfo
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string

	for _, p := range allProfiles {
		wg.Add(1)
		go func(prof *profile.Profile) {
			defer wg.Done()
			clusters, err := s.getClustersForProfile(prof)
			if err == nil {
				applyAliases(clusters, prof.ClusterAliases)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				log.Printf("Error getting clusters for profile %s: %v", prof.Name, err)
				failures = append(failures, fmt.Sprintf("%s: %v", prof.Name, err))
				return
			}
			allClusters = append(allClusters, clusters...)
		}(p)
	}

	wg.Wait()

	sort.SliceStable(allClusters, func(i, j int) bool {
		a, b := allClusters[i], allClusters[j]
		if a.ProfileName != b.ProfileName {
			return a.ProfileName < b.ProfileName
		}
		if a.ProfileID != b.ProfileID {
			return a.ProfileID < b.ProfileID
		}
		return a.Name < b.Name
	})
	sort.Strings(failures)

	// Mark clusters that exist in local kubeconfig
	s.markClustersInLocalKubeconfig(allClusters)

	// Return clusters even if some profiles failed
	response := APIResponse{
		Success: true,
		Data:    allClusters,
	}
	if len(failures) > 0 {
		response.Error = fmt.Sprintf("Some sources failed: %s", strings.Join(failures, "; "))
		response.Warnings = failures
	}

	s.writeJSON(w, http.StatusOK, response)
}

// generateOptions are the output options shared by the generate endpoints.
type generateOptions struct {
	ClusterPrefix     string   `json:"clusterPrefix"`
	AptakubeTags      []string `json:"aptakubeTags"`
	SeparateFiles     bool     `json:"separateFiles"`
	UseSourcePrefix   bool     `json:"useSourcePrefix"`
	MergeToKubeconfig bool     `json:"mergeToKubeconfig"`
}

// fetchedKubeconfig is one selected cluster's kubeconfig together with the
// unique name (generator input key) it is exported under.
type fetchedKubeconfig struct {
	Key        string
	ProfileID  string
	ClusterID  string
	Kubeconfig string
}

// handleGenerateForAllSources generates kubeconfig for selected clusters from any source
func (s *Server) handleGenerateForAllSources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w)
		return
	}

	var req struct {
		SelectedClusters []ClusterSelection `json:"selectedClusters"`
		generateOptions
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	if !s.storeAvailable(w) {
		return
	}

	// Group clusters by profile, keeping the order of the request so that
	// collision handling is deterministic.
	var profileOrder []string
	clustersByProfile := make(map[string][]ClusterSelection)
	for _, c := range req.SelectedClusters {
		if _, seen := clustersByProfile[c.ProfileID]; !seen {
			profileOrder = append(profileOrder, c.ProfileID)
		}
		clustersByProfile[c.ProfileID] = append(clustersByProfile[c.ProfileID], c)
	}

	var entries []fetchedKubeconfig
	var failures []ClusterFailure
	taken := make(map[string]bool)
	for _, profileID := range profileOrder {
		clusters := clustersByProfile[profileID]
		p, err := s.profileStore.Get(profileID)
		if err != nil {
			for _, c := range clusters {
				failures = append(failures, ClusterFailure{Cluster: c.DisplayName(), Error: err.Error()})
			}
			continue
		}

		fetched, profFailures := s.getKubeconfigsForProfile(p, clusters)
		for i := range profFailures {
			profFailures[i].Cluster = fmt.Sprintf("%s (%s)", profFailures[i].Cluster, p.Name)
		}
		failures = append(failures, profFailures...)

		sourcePrefix := ""
		if req.UseSourcePrefix {
			sourcePrefix = sourcePrefixFor(p.Name)
		}
		for _, f := range fetched {
			key := sourcePrefix + f.Key
			if taken[key] {
				// Same display name from another source: qualify it with the
				// profile name rather than silently replacing the first one.
				qualified := key
				if suffix := sanitizeSourceName(p.Name); suffix != "" {
					qualified = key + "-" + suffix
				}
				qualified = uniqueKey(qualified, func(k string) bool { return taken[k] })
				log.Printf("Cluster name %q is already used by another selected cluster; exporting %s from %q as %q", key, f.ClusterID, p.Name, qualified)
				key = qualified
			}
			taken[key] = true
			f.Key = key
			entries = append(entries, f)
		}
	}

	s.respondGenerated(w, req.generateOptions, entries, failures)
}

// respondGenerated builds the kubeconfig(s) for entries and either merges
// them into the local kubeconfig or returns them as a download.
func (s *Server) respondGenerated(w http.ResponseWriter, opts generateOptions, entries []fetchedKubeconfig, failures []ClusterFailure) {
	failureStrings := make([]string, len(failures))
	for i, f := range failures {
		failureStrings[i] = fmt.Sprintf("%s: %s", f.Cluster, f.Error)
	}

	if len(entries) == 0 {
		msg := "No kubeconfigs retrieved for selected clusters"
		if len(failureStrings) > 0 {
			msg += ": " + strings.Join(failureStrings, "; ")
		}
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success:  false,
			Error:    msg,
			Warnings: failureStrings,
		})
		return
	}

	kubeconfigs := make(map[string]string, len(entries))
	for _, e := range entries {
		kubeconfigs[e.Key] = e.Kubeconfig
	}

	generator := kubeconfig.NewGenerator(opts.ClusterPrefix)
	if len(opts.AptakubeTags) > 0 {
		generator.SetAllTags(opts.AptakubeTags)
	}

	switch {
	case opts.MergeToKubeconfig:
		s.mergeGenerated(w, generator, entries, kubeconfigs, failures)

	case opts.SeparateFiles:
		zipData, err := buildZip(generator, kubeconfigs)
		if err != nil {
			s.writeJSON(w, http.StatusInternalServerError, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("Failed to create zip file: %v", err),
			})
			return
		}
		setWarningsHeader(w, failureStrings)
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", "attachment; filename=kubeconfigs.zip")
		if _, err := w.Write(zipData); err != nil {
			log.Printf("Error writing zip response: %v", err)
		}

	default:
		kubeconfigData, err := generator.Generate(kubeconfigs)
		if err != nil {
			s.writeJSON(w, http.StatusInternalServerError, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("Failed to generate kubeconfig: %v", err),
			})
			return
		}
		setWarningsHeader(w, failureStrings)
		w.Header().Set("Content-Type", "application/x-yaml")
		w.Header().Set("Content-Disposition", "attachment; filename=kubeconfig.yaml")
		if _, err := w.Write(kubeconfigData); err != nil {
			log.Printf("Error writing kubeconfig response: %v", err)
		}
	}
}

// mergeGenerated merges the generated kubeconfig into the local kubeconfig
// and records export tracking for the clusters whose contexts were written.
func (s *Server) mergeGenerated(w http.ResponseWriter, generator *kubeconfig.Generator, entries []fetchedKubeconfig, kubeconfigs map[string]string, failures []ClusterFailure) {
	data, names, err := generator.GenerateWithNames(kubeconfigs)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to generate kubeconfig: %v", err),
		})
		return
	}

	added, replaced, err := s.ctxSwitcher.MergeKubeconfigReport(string(data))
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to merge kubeconfig: %v", err),
		})
		return
	}

	written := make(map[string]bool, len(added)+len(replaced))
	for _, n := range added {
		written[n] = true
	}
	for _, n := range replaced {
		written[n] = true
	}
	for _, e := range entries {
		ctxNames := names[e.Key]
		if len(ctxNames) == 0 || !written[ctxNames[0]] || e.ProfileID == "" || e.ClusterID == "" {
			continue
		}
		if err := s.profileStore.SetExportedContext(e.ProfileID, e.ClusterID, ctxNames[0]); err != nil {
			log.Printf("Failed to track export of cluster %s: %v", e.ClusterID, err)
		}
	}

	result := MergeResult{Added: added, Replaced: replaced, Failed: failures}
	if result.Added == nil {
		result.Added = []string{}
	}
	if result.Replaced == nil {
		result.Replaced = []string{}
	}
	if result.Failed == nil {
		result.Failed = []ClusterFailure{}
	}
	s.writeJSON(w, http.StatusOK, APIResponse{Success: true, Data: result})
}

// buildZip generates one kubeconfig per entry and packs them into a zip with
// portable, unique file names.
func buildZip(generator *kubeconfig.Generator, kubeconfigs map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(kubeconfigs))
	for k := range kubeconfigs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)
	used := make(map[string]bool)
	for _, clusterName := range keys {
		data, err := generator.Generate(map[string]string{clusterName: kubeconfigs[clusterName]})
		if err != nil {
			log.Printf("Error generating kubeconfig for %s: %v", clusterName, err)
			continue
		}
		// Compare case-insensitively: Windows and macOS file systems do.
		base := uniqueKey(sanitizeZipName(clusterName), func(n string) bool { return used[strings.ToLower(n)] })
		used[strings.ToLower(base)] = true

		fw, err := zw.Create(base + ".yaml")
		if err != nil {
			return nil, err
		}
		if _, err := fw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// handleAWSProfiles returns available AWS CLI profiles
func (s *Server) handleAWSProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}

	profiles, err := provider.ListAWSProfiles()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to list AWS profiles: %v", err),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    profiles,
	})
}

// handleAWSRegions returns available AWS regions
func (s *Server) handleAWSRegions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}

	regions := provider.ListAWSRegions()
	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    regions,
	})
}

// handleOCIProfiles returns the profiles of the OCI config file
func (s *Server) handleOCIProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}

	profiles, err := provider.ListOCIProfiles()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to list OCI profiles: %v", err),
		})
		return
	}
	if profiles == nil {
		profiles = []string{}
	}

	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    profiles,
	})
}

// handleOCIRegions returns the OCI commercial regions
func (s *Server) handleOCIRegions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}

	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    provider.ListOCIRegions(),
	})
}

// handleContext handles getting/setting the current context
func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ctx, err := s.ctxSwitcher.GetCurrentContext()
		if err != nil {
			s.writeJSON(w, http.StatusInternalServerError, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("Failed to get current context: %v", err),
			})
			return
		}
		s.writeJSON(w, http.StatusOK, APIResponse{
			Success: true,
			Data: map[string]string{
				"currentContext": ctx,
				"kubeconfigPath": s.ctxSwitcher.Path(),
			},
		})

	case http.MethodPut:
		var req struct {
			Context string `json:"context"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("Invalid request: %v", err),
			})
			return
		}

		if err := s.ctxSwitcher.SetCurrentContext(req.Context); err != nil {
			s.writeJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("Failed to set context: %v", err),
			})
			return
		}
		s.writeJSON(w, http.StatusOK, APIResponse{
			Success: true,
		})

	default:
		s.methodNotAllowed(w)
	}
}

// handleContexts returns all available contexts
func (s *Server) handleContexts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}

	contexts, err := s.ctxSwitcher.ListContexts()
	if err != nil {
		// If kubeconfig doesn't exist, return empty list
		if !s.ctxSwitcher.Exists() {
			s.writeJSON(w, http.StatusOK, APIResponse{
				Success: true,
				Data:    []kctx.ContextInfo{},
			})
			return
		}
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to list contexts: %v", err),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    contexts,
	})
}

// handleContextDelete deletes a context from the kubeconfig
func (s *Server) handleContextDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w)
		return
	}

	var req struct {
		Name           string `json:"name"`
		CleanupOrphans bool   `json:"cleanupOrphans"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	if req.Name == "" {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "Context name is required",
		})
		return
	}

	if err := s.ctxSwitcher.DeleteContextWithCleanup(req.Name, req.CleanupOrphans); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to delete context: %v", err),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
	})
}

// handleContextRename renames a context in the kubeconfig
func (s *Server) handleContextRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w)
		return
	}

	var req struct {
		OldName string `json:"oldName"`
		NewName string `json:"newName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	if req.OldName == "" || req.NewName == "" {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "Both old and new context names are required",
		})
		return
	}

	if err := s.ctxSwitcher.RenameContext(req.OldName, req.NewName); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to rename context: %v", err),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
	})
}

// handleClusterAlias sets or removes a cluster alias for a profile
func (s *Server) handleClusterAlias(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w)
		return
	}

	if !s.storeAvailable(w) {
		return
	}

	var req struct {
		ProfileID string `json:"profileId"`
		ClusterID string `json:"clusterId"`
		Alias     string `json:"alias"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	if req.ProfileID == "" || req.ClusterID == "" {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "Profile ID and cluster ID are required",
		})
		return
	}

	if err := s.profileStore.SetClusterAlias(req.ProfileID, req.ClusterID, req.Alias); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to set cluster alias: %v", err),
		})
		return
	}

	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
	})
}

// handleGenerateForProfile generates a kubeconfig for selected clusters from a profile
func (s *Server) handleGenerateForProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w)
		return
	}

	var req struct {
		ProfileID        string             `json:"profileId"`
		SelectedClusters []ClusterSelection `json:"selectedClusters"`
		generateOptions
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	if !s.storeAvailable(w) {
		return
	}

	p, err := s.profileStore.Get(req.ProfileID)
	if err != nil {
		s.writeJSON(w, http.StatusNotFound, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	fetched, failures := s.getKubeconfigsForProfile(p, req.SelectedClusters)

	sourcePrefix := ""
	if req.UseSourcePrefix {
		sourcePrefix = sourcePrefixFor(p.Name)
	}
	taken := make(map[string]bool, len(fetched))
	for i := range fetched {
		key := sourcePrefix + fetched[i].Key
		unique := uniqueKey(key, func(k string) bool { return taken[k] })
		if unique != key {
			log.Printf("Cluster name %q is used by more than one selected cluster; exporting %s as %q", key, fetched[i].ClusterID, unique)
		}
		taken[unique] = true
		fetched[i].Key = unique
	}

	s.respondGenerated(w, req.generateOptions, fetched, failures)
}

// getKubeconfigsForProfile fetches the kubeconfigs of the selected clusters
// of one profile. Each result's Key is the cluster's display name (alias or
// name), not yet unique. Clusters that could not be fetched, or every
// cluster when the provider cannot be created, are returned as failures.
func (s *Server) getKubeconfigsForProfile(p *profile.Profile, selectedClusters []ClusterSelection) ([]fetchedKubeconfig, []ClusterFailure) {
	var failures []ClusterFailure
	prov, err := newProviderForProfile(p)
	if err != nil {
		log.Printf("Cannot use profile %s: %v", p.Name, err)
		for _, c := range selectedClusters {
			failures = append(failures, ClusterFailure{Cluster: c.DisplayName(), Error: err.Error()})
		}
		return nil, failures
	}
	defer prov.Close()

	var fetched []fetchedKubeconfig
	for _, cluster := range selectedClusters {
		// Use ID for API call, DisplayName (alias or name) for labeling
		kc, err := prov.GetKubeconfig(cluster.ID)
		if err != nil {
			log.Printf("Warning: failed to get kubeconfig for cluster %s (id: %s): %v", cluster.Name, cluster.ID, err)
			failures = append(failures, ClusterFailure{Cluster: cluster.DisplayName(), Error: err.Error()})
			continue
		}
		fetched = append(fetched, fetchedKubeconfig{
			Key:        cluster.DisplayName(),
			ProfileID:  p.ID,
			ClusterID:  cluster.ID,
			Kubeconfig: kc,
		})
	}
	return fetched, failures
}

// indexHTML is the embedded HTML template for the web interface
const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <meta name="auth-token" content="{{.AuthToken}}">
    <title>Kubeconfig Wrangler</title>
    <style>
        :root {
            --bg-primary: #1e1e1e;
            --bg-secondary: #252526;
            --bg-tertiary: #2d2d30;
            --bg-hover: #3c3c3c;
            --bg-active: #094771;
            --text-primary: #cccccc;
            --text-secondary: #969696;
            --text-bright: #ffffff;
            --accent: #0078d4;
            --accent-hover: #1c8ae6;
            --success: #4ec9b0;
            --warning: #dcdcaa;
            --error: #f14c4c;
            --border: #3c3c3c;
            --input-bg: #3c3c3c;
            --sidebar-width: 320px;
        }

        @media (prefers-color-scheme: light) {
            :root {
                --bg-primary: #ffffff;
                --bg-secondary: #f3f3f3;
                --bg-tertiary: #e8e8e8;
                --bg-hover: #e0e0e0;
                --bg-active: #cce4f7;
                --text-primary: #333333;
                --text-secondary: #666666;
                --text-bright: #000000;
                --accent: #0078d4;
                --accent-hover: #106ebe;
                --success: #107c10;
                --warning: #ca8a00;
                --error: #d32f2f;
                --border: #d4d4d4;
                --input-bg: #ffffff;
            }
        }

        * {
            box-sizing: border-box;
            margin: 0;
            padding: 0;
        }

        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
            background: var(--bg-primary);
            color: var(--text-primary);
            font-size: 13px;
            line-height: 1.4;
            height: 100vh;
            overflow: hidden;
            display: flex;
            flex-direction: column;
        }

        /* Toolbar */
        .toolbar {
            height: 38px;
            background: var(--bg-secondary);
            border-bottom: 1px solid var(--border);
            display: flex;
            align-items: center;
            padding: 0 12px;
            gap: 8px;
            -webkit-app-region: drag;
            flex-shrink: 0;
        }

        .toolbar-title {
            font-weight: 600;
            font-size: 13px;
            color: var(--text-bright);
            margin-right: auto;
        }

        .toolbar-btn {
            -webkit-app-region: no-drag;
            background: var(--accent);
            color: white;
            border: none;
            padding: 5px 12px;
            border-radius: 3px;
            font-size: 12px;
            font-weight: 500;
            cursor: pointer;
            display: flex;
            align-items: center;
            gap: 6px;
        }

        .toolbar-btn:hover {
            background: var(--accent-hover);
        }

        .toolbar-btn:disabled {
            background: var(--bg-tertiary);
            color: var(--text-secondary);
            cursor: not-allowed;
        }

        .toolbar-btn-secondary {
            background: var(--bg-tertiary);
            color: var(--text-primary);
        }

        .toolbar-btn-secondary:hover {
            background: var(--bg-hover);
        }

        /* Main Layout */
        .main-container {
            display: flex;
            flex: 1;
            overflow: hidden;
        }

        /* Sidebar */
        .sidebar {
            width: var(--sidebar-width);
            background: var(--bg-secondary);
            border-right: 1px solid var(--border);
            display: flex;
            flex-direction: column;
            flex-shrink: 0;
        }

        .sidebar-header {
            padding: 12px;
            font-weight: 600;
            font-size: 11px;
            text-transform: uppercase;
            letter-spacing: 0.5px;
            color: var(--text-secondary);
            border-bottom: 1px solid var(--border);
            display: flex;
            justify-content: space-between;
            align-items: center;
        }

        .add-btn {
            background: var(--accent);
            color: white;
            border: none;
            width: 20px;
            height: 20px;
            border-radius: 3px;
            font-size: 14px;
            font-weight: bold;
            cursor: pointer;
            display: flex;
            align-items: center;
            justify-content: center;
        }

        .add-btn:hover {
            background: var(--accent-hover);
        }

        .sidebar-content {
            flex: 1;
            overflow-y: auto;
            padding: 12px;
        }

        .sidebar-divider {
            height: 1px;
            background: var(--border);
            margin: 16px 0;
        }

        .profiles-section {
            margin-bottom: 8px;
        }

        .profile-list {
            display: flex;
            flex-direction: column;
            gap: 4px;
        }

        .empty-profiles {
            color: var(--text-secondary);
            font-size: 11px;
            font-style: italic;
            padding: 8px 0;
        }

        .profile-item {
            display: flex;
            align-items: center;
            gap: 8px;
            padding: 8px;
            background: var(--bg-tertiary);
            border-radius: 4px;
            cursor: pointer;
        }

        .profile-item:hover {
            background: var(--bg-hover);
        }

        .profile-item.active {
            background: var(--bg-active);
        }

        .profile-separator {
            height: 1px;
            background: var(--border);
            margin: 4px 8px;
        }

        .profile-icon {
            width: 20px;
            height: 20px;
            display: flex;
            align-items: center;
            justify-content: center;
            font-size: 12px;
        }

        .profile-info {
            flex: 1;
            min-width: 0;
        }

        .profile-name {
            font-size: 12px;
            font-weight: 500;
            color: var(--text-primary);
            white-space: nowrap;
            overflow: hidden;
            text-overflow: ellipsis;
        }

        .profile-type {
            font-size: 10px;
            color: var(--text-secondary);
        }

        .profile-actions {
            display: flex;
            gap: 2px;
            opacity: 0;
            transition: opacity 0.1s;
        }

        .profile-item:hover .profile-actions {
            opacity: 1;
        }

        .profile-action-btn {
            background: none;
            border: none;
            color: var(--text-secondary);
            cursor: pointer;
            padding: 4px;
            font-size: 12px;
            border-radius: 3px;
        }

        .profile-action-btn:hover {
            background: var(--bg-hover);
            color: var(--text-primary);
        }

        .profile-action-btn.delete:hover {
            color: var(--error);
        }

        /* Modal */
        .modal-overlay {
            position: fixed;
            top: 0;
            left: 0;
            right: 0;
            bottom: 0;
            background: rgba(0, 0, 0, 0.5);
            display: none;
            align-items: center;
            justify-content: center;
            z-index: 1000;
        }

        .modal-overlay.show {
            display: flex;
        }

        .modal {
            background: var(--bg-secondary);
            border-radius: 8px;
            width: 400px;
            max-height: 80vh;
            overflow: hidden;
            box-shadow: 0 4px 20px rgba(0, 0, 0, 0.3);
        }

        .modal-header {
            padding: 16px;
            border-bottom: 1px solid var(--border);
            display: flex;
            justify-content: space-between;
            align-items: center;
        }

        .modal-title {
            font-size: 14px;
            font-weight: 600;
            color: var(--text-bright);
        }

        .modal-close {
            background: none;
            border: none;
            color: var(--text-secondary);
            cursor: pointer;
            font-size: 18px;
            padding: 4px;
        }

        .modal-close:hover {
            color: var(--text-primary);
        }

        .modal-body {
            padding: 16px;
            overflow-y: auto;
            max-height: 60vh;
        }

        .modal-footer {
            padding: 16px;
            border-top: 1px solid var(--border);
            display: flex;
            justify-content: flex-end;
            gap: 8px;
        }

        .type-tabs {
            display: flex;
            gap: 4px;
            margin-bottom: 16px;
        }

        .type-tab {
            flex: 1;
            padding: 8px;
            background: var(--bg-tertiary);
            border: none;
            border-radius: 4px;
            color: var(--text-secondary);
            font-size: 12px;
            cursor: pointer;
            text-align: center;
        }

        .type-tab:hover {
            background: var(--bg-hover);
        }

        .type-tab.active {
            background: var(--accent);
            color: white;
        }

        .type-panel {
            display: none;
        }

        .type-panel.active {
            display: block;
        }

        .form-group select,
        .form-group textarea {
            width: 100%;
            padding: 6px 8px;
            background: var(--input-bg);
            border: 1px solid var(--border);
            border-radius: 3px;
            color: var(--text-primary);
            font-size: 12px;
            font-family: inherit;
        }

        .form-group textarea {
            min-height: 100px;
            resize: vertical;
            font-family: monospace;
        }

        .form-group select:focus,
        .form-group textarea:focus {
            outline: none;
            border-color: var(--accent);
        }

        .form-section {
            margin-bottom: 16px;
        }

        .form-section-title {
            font-size: 11px;
            font-weight: 600;
            color: var(--text-secondary);
            text-transform: uppercase;
            letter-spacing: 0.5px;
            margin-bottom: 8px;
        }

        .form-group {
            margin-bottom: 12px;
        }

        .form-group label {
            display: block;
            font-size: 12px;
            color: var(--text-secondary);
            margin-bottom: 4px;
        }

        .form-group input[type="text"],
        .form-group input[type="password"],
        .form-group input[type="url"] {
            width: 100%;
            padding: 6px 8px;
            background: var(--input-bg);
            border: 1px solid var(--border);
            border-radius: 3px;
            color: var(--text-primary);
            font-size: 12px;
            font-family: inherit;
        }

        .form-group input:focus {
            outline: none;
            border-color: var(--accent);
        }

        .form-group input::placeholder {
            color: var(--text-secondary);
        }

        /* Segmented Control */
        .segmented-control {
            display: flex;
            background: var(--bg-tertiary);
            border-radius: 4px;
            padding: 2px;
            margin-bottom: 12px;
        }

        .segment-btn {
            flex: 1;
            padding: 5px 8px;
            border: none;
            background: transparent;
            color: var(--text-secondary);
            font-size: 11px;
            font-weight: 500;
            cursor: pointer;
            border-radius: 3px;
            transition: all 0.15s;
        }

        .segment-btn:hover {
            color: var(--text-primary);
        }

        .segment-btn.active {
            background: var(--accent);
            color: white;
        }

        .auth-panel {
            display: none;
        }

        .auth-panel.active {
            display: block;
        }

        /* Checkbox */
        .checkbox-row {
            display: flex;
            align-items: center;
            gap: 8px;
            padding: 4px 0;
        }

        .checkbox-row input[type="checkbox"] {
            width: 14px;
            height: 14px;
            accent-color: var(--accent);
        }

        .checkbox-row label {
            font-size: 12px;
            color: var(--text-primary);
            cursor: pointer;
            margin: 0;
        }

        /* Main Content */
        .content {
            flex: 1;
            display: flex;
            flex-direction: column;
            overflow: hidden;
        }

        .content-header {
            padding: 12px 16px;
            background: var(--bg-secondary);
            border-bottom: 1px solid var(--border);
            display: flex;
            align-items: center;
            justify-content: space-between;
        }

        .content-title {
            font-weight: 600;
            font-size: 12px;
            color: var(--text-bright);
        }

        .cluster-count {
            font-size: 11px;
            color: var(--text-secondary);
            background: var(--bg-tertiary);
            padding: 2px 8px;
            border-radius: 10px;
        }

        /* Table */
        .table-container {
            flex: 1;
            overflow: auto;
        }

        .cluster-table {
            width: 100%;
            border-collapse: collapse;
        }

        .cluster-table th {
            position: sticky;
            top: 0;
            background: var(--bg-tertiary);
            text-align: left;
            padding: 8px 12px;
            font-size: 11px;
            font-weight: 600;
            color: var(--text-secondary);
            text-transform: uppercase;
            letter-spacing: 0.5px;
            border-bottom: 1px solid var(--border);
        }

        .cluster-table th:first-child {
            width: 40px;
            text-align: center;
        }

        .cluster-table td {
            padding: 10px 12px;
            border-bottom: 1px solid var(--border);
            vertical-align: middle;
        }

        .cluster-table tr:hover {
            background: var(--bg-hover);
        }

        .cluster-table tr.selected {
            background: var(--bg-active);
        }

        .cluster-table td:first-child {
            text-align: center;
        }

        .cluster-name {
            font-weight: 500;
            color: var(--text-bright);
        }

        .cluster-name-cell {
            padding: 8px 12px;
        }

        .cluster-name-row {
            display: flex;
            align-items: center;
            gap: 4px;
        }

        .cluster-display-name {
            color: var(--text-primary);
            font-weight: 500;
        }

        .cluster-source-name {
            font-size: 11px;
            color: var(--text-secondary);
            margin-top: 2px;
            font-family: monospace;
        }

        .edit-alias-btn {
            background: transparent;
            border: none;
            cursor: pointer;
            padding: 2px 4px;
            font-size: 11px;
            opacity: 0.4;
            transition: opacity 0.2s;
        }

        .edit-alias-btn:hover {
            opacity: 1;
        }

        .cluster-id {
            font-size: 11px;
            color: var(--text-secondary);
            font-family: 'SF Mono', Monaco, 'Courier New', monospace;
        }

        .status-badge {
            display: inline-flex;
            align-items: center;
            gap: 4px;
            padding: 2px 8px;
            border-radius: 10px;
            font-size: 11px;
            font-weight: 500;
        }

        .status-badge::before {
            content: '';
            width: 6px;
            height: 6px;
            border-radius: 50%;
        }

        .status-active {
            background: rgba(78, 201, 176, 0.15);
            color: var(--success);
        }

        .status-active::before {
            background: var(--success);
        }

        .status-inactive {
            background: rgba(220, 220, 170, 0.15);
            color: var(--warning);
        }

        .status-inactive::before {
            background: var(--warning);
        }

        .status-error {
            background: rgba(241, 76, 76, 0.15);
            color: var(--error);
        }

        .status-error::before {
            background: var(--error);
        }

        /* Local kubeconfig indicator */
        .local-config-badge {
            display: inline-flex;
            align-items: center;
            gap: 3px;
            padding: 1px 6px;
            border-radius: 8px;
            font-size: 10px;
            font-weight: 500;
            background: rgba(78, 201, 176, 0.15);
            color: var(--success);
            margin-left: 8px;
        }

        .local-config-badge::before {
            content: '✓';
        }

        /* Empty State */
        .empty-state {
            display: flex;
            flex-direction: column;
            align-items: center;
            justify-content: center;
            height: 100%;
            color: var(--text-secondary);
            text-align: center;
            padding: 40px;
        }

        .empty-state-icon {
            font-size: 48px;
            margin-bottom: 16px;
            opacity: 0.5;
        }

        .empty-state-title {
            font-size: 16px;
            font-weight: 500;
            color: var(--text-primary);
            margin-bottom: 8px;
        }

        .empty-state-text {
            font-size: 13px;
            max-width: 300px;
        }

        /* Loading */
        .loading-overlay {
            display: none;
            position: absolute;
            top: 0;
            left: 0;
            right: 0;
            bottom: 0;
            background: rgba(0, 0, 0, 0.5);
            align-items: center;
            justify-content: center;
            z-index: 100;
        }

        .loading-overlay.visible {
            display: flex;
        }

        .loading-spinner {
            width: 32px;
            height: 32px;
            border: 3px solid var(--border);
            border-top-color: var(--accent);
            border-radius: 50%;
            animation: spin 0.8s linear infinite;
        }

        @keyframes spin {
            to { transform: rotate(360deg); }
        }

        /* Options Panel */
        .options-panel {
            background: var(--bg-secondary);
            border-top: 1px solid var(--border);
            margin-top: auto;
        }
        .options-header {
            display: flex;
            justify-content: space-between;
            align-items: center;
            padding: 8px 16px;
            cursor: pointer;
            font-size: 12px;
            font-weight: 500;
            color: var(--text-secondary);
        }
        .options-header:hover {
            background: var(--bg-tertiary);
        }
        .options-toggle {
            transition: transform 0.2s;
        }
        .options-panel.collapsed .options-toggle {
            transform: rotate(-90deg);
        }
        .options-panel.collapsed .options-content {
            display: none;
        }
        .options-content {
            padding: 12px 16px;
            border-top: 1px solid var(--border);
        }
        .options-row {
            display: flex;
            gap: 16px;
            margin-bottom: 12px;
        }
        .options-row:last-child {
            margin-bottom: 0;
        }
        .option-group {
            flex: 1;
        }
        .option-group label {
            display: block;
            font-size: 11px;
            font-weight: 500;
            color: var(--text-secondary);
            margin-bottom: 4px;
        }
        .option-group input[type="text"] {
            width: 100%;
            padding: 6px 8px;
            background: var(--bg-primary);
            border: 1px solid var(--border);
            border-radius: 4px;
            color: var(--text-primary);
            font-size: 12px;
        }
        .option-group input[type="text"]:focus {
            outline: none;
            border-color: var(--accent);
        }
        .option-hint {
            display: block;
            font-size: 10px;
            color: var(--text-muted);
            margin-top: 4px;
        }
        .prefix-preview {
            font-size: 11px;
            color: var(--text-secondary);
            margin-top: 4px;
            padding: 4px 8px;
            background: var(--bg-tertiary);
            border-radius: 3px;
            font-family: monospace;
            display: none;
        }
        .prefix-preview.visible {
            display: block;
        }
        .prefix-preview .prefix-highlight {
            color: var(--accent);
            font-weight: 500;
        }
        .option-checkbox label {
            display: flex;
            align-items: center;
            gap: 8px;
            cursor: pointer;
            font-size: 12px;
            color: var(--text-primary);
        }
        .option-checkbox input[type="checkbox"] {
            width: 14px;
            height: 14px;
            cursor: pointer;
        }

        /* Status Bar */
        .status-bar {
            height: 24px;
            background: var(--bg-tertiary);
            border-top: 1px solid var(--border);
            display: flex;
            align-items: center;
            padding: 0 12px;
            font-size: 11px;
            color: var(--text-secondary);
            flex-shrink: 0;
        }

        .status-bar-item {
            display: flex;
            align-items: center;
            gap: 6px;
        }

        .status-bar-item::after {
            content: '|';
            margin: 0 8px;
            opacity: 0.3;
        }

        .status-bar-item:last-child::after {
            display: none;
        }

        .status-dot {
            width: 8px;
            height: 8px;
            border-radius: 50%;
            background: var(--text-secondary);
        }

        .status-dot.connected {
            background: var(--success);
        }

        .status-dot.error {
            background: var(--error);
        }

        /* Toast Notification */
        .toast {
            position: fixed;
            bottom: 40px;
            left: 50%;
            transform: translateX(-50%) translateY(100px);
            background: var(--bg-tertiary);
            border: 1px solid var(--border);
            border-radius: 6px;
            padding: 12px 20px;
            font-size: 13px;
            box-shadow: 0 4px 12px rgba(0, 0, 0, 0.3);
            z-index: 1000;
            opacity: 0;
            transition: all 0.3s ease;
        }

        .toast.visible {
            transform: translateX(-50%) translateY(0);
            opacity: 1;
        }

        .toast.success {
            border-left: 3px solid var(--success);
        }

        .toast.error {
            border-left: 3px solid var(--error);
        }

        .toast {
            max-width: 80vw;
            white-space: pre-line;
            word-break: break-word;
        }

        .toast.warning {
            border-left: 3px solid var(--warning);
        }

        .status-dot.warning {
            background: var(--warning);
        }

        .add-btn:disabled {
            opacity: 0.4;
            cursor: not-allowed;
        }

        .sidebar-notice {
            margin: 8px;
            padding: 8px 10px;
            font-size: 12px;
            background: var(--bg-tertiary);
            border-left: 3px solid var(--error);
            border-radius: 3px;
            word-break: break-word;
        }

        .profile-item.locked .profile-name {
            color: var(--text-secondary);
        }

        .profile-lock {
            margin-right: 4px;
        }

        .notice {
            display: flex;
            align-items: flex-start;
            gap: 8px;
            margin: 8px 12px;
            padding: 8px 12px;
            font-size: 12px;
            background: var(--bg-tertiary);
            border-left: 3px solid var(--warning);
            border-radius: 3px;
        }

        .notice.modal-notice {
            margin: 0 0 12px 0;
        }

        .notice-body {
            flex: 1;
            min-width: 0;
            word-break: break-word;
        }

        .notice-body ul {
            margin: 4px 0 0 18px;
        }

        .notice-close {
            background: none;
            border: none;
            color: var(--text-secondary);
            cursor: pointer;
            font-size: 16px;
            line-height: 1;
        }

        .notice-close:hover {
            color: var(--text-bright);
        }

        .hidden {
            display: none !important;
        }
    </style>
</head>
<body>
    <!-- Toolbar -->
    <div class="toolbar">
        <span class="toolbar-title">Kubeconfig Wrangler</span>
        <button class="toolbar-btn toolbar-btn-secondary" onclick="showManageKubeconfigModal()">
            ⚙ Manage Kubeconfig
        </button>
        <button class="toolbar-btn toolbar-btn-secondary" id="refreshBtn" onclick="fetchClusters()" disabled>
            ↻ Refresh
        </button>
        <button class="toolbar-btn" id="generateBtn" onclick="generateKubeconfig()" disabled>
            ⬇ Generate Kubeconfig
        </button>
    </div>

    <!-- Main Container -->
    <div class="main-container">
        <!-- Sidebar -->
        <div class="sidebar">
            <div class="sidebar-header">
                Connections
                <button class="add-btn" id="addConnectionBtn" onclick="showAddConnectionModal()" title="Add Connection">+</button>
            </div>
            <div class="sidebar-content">
                <div class="sidebar-notice hidden" id="storeError"></div>
                <!-- Saved Profiles -->
                <div class="profiles-section" id="profilesSection">
                    <div class="profile-list" id="profileList">
                        <div class="empty-profiles">No saved connections. Click + to add one.</div>
                    </div>
                </div>
            </div>
        </div>

        <!-- Content -->
        <div class="content">
            <div class="content-header">
                <span class="content-title">Clusters</span>
                <span class="cluster-count" id="clusterCount">0 clusters</span>
            </div>

            <div class="notice hidden" id="loadWarningsBanner">
                <div class="notice-body">
                    <strong>Some saved connections need attention.</strong>
                    Connections marked 🔒 could not be decrypted; edit them and re-enter their credentials.
                    <ul id="loadWarningsList"></ul>
                </div>
                <button class="notice-close" onclick="hideNotice('loadWarningsBanner')" title="Dismiss">&times;</button>
            </div>

            <div class="notice hidden" id="clusterWarningsNotice">
                <div class="notice-body">
                    <strong>Some sources failed to load; their clusters are not listed.</strong>
                    <ul id="clusterWarningsList"></ul>
                </div>
                <button class="notice-close" onclick="hideNotice('clusterWarningsNotice')" title="Dismiss">&times;</button>
            </div>

            <div class="table-container" id="tableContainer">
                <!-- Empty State -->
                <div class="empty-state" id="emptyState">
                    <div class="empty-state-icon">☁</div>
                    <div class="empty-state-title">No clusters loaded</div>
                    <div class="empty-state-text">Add a connection using the + button, then select it to load clusters.</div>
                </div>

                <!-- Cluster Table -->
                <table class="cluster-table hidden" id="clusterTable">
                    <thead>
                        <tr>
                            <th><input type="checkbox" id="selectAll" onchange="toggleSelectAll()"></th>
                            <th>Cluster</th>
                            <th>Source</th>
                            <th>Provider</th>
                            <th>Status</th>
                        </tr>
                    </thead>
                    <tbody id="clusterTableBody">
                    </tbody>
                </table>
            </div>

            <!-- Options Panel -->
            <div class="options-panel" id="optionsPanel">
                <div class="options-header" onclick="toggleOptionsPanel()">
                    <span>Generation Options</span>
                    <span class="options-toggle" id="optionsToggle">▼</span>
                </div>
                <div class="options-content" id="optionsContent">
                    <div class="options-row">
                        <div class="option-group">
                            <label>Cluster Name Prefix</label>
                            <input type="text" id="clusterPrefix" placeholder="e.g., prod-" oninput="updatePrefixPreview()">
                            <div class="prefix-preview" id="prefixPreview"></div>
                        </div>
                        <div class="option-group">
                            <label>Aptakube Tags</label>
                            <input type="text" id="aptakubeTags" placeholder="tag1, tag2, tag3">
                            <span class="option-hint">Comma-separated tags for Aptakube</span>
                        </div>
                    </div>
                    <div class="options-row">
                        <div class="option-group option-checkbox">
                            <label>
                                <input type="checkbox" id="useSourcePrefix" onchange="updatePrefixPreview()">
                                Use source name as prefix
                            </label>
                            <span class="option-hint">Prefix cluster contexts with source name (e.g., my-rancher-cluster-name)</span>
                        </div>
                        <div class="option-group option-checkbox">
                            <label>
                                <input type="checkbox" id="separateFiles">
                                Generate one file per cluster (zip)
                            </label>
                            <span class="option-hint">Download a zip with one kubeconfig file per selected cluster</span>
                        </div>
                    </div>
                    <div class="options-row">
                        <div class="option-group option-checkbox">
                            <label>
                                <input type="checkbox" id="mergeToKubeconfig">
                                Merge into local kubeconfig
                            </label>
                            <span class="option-hint">Merge generated contexts directly into <span id="mergeTargetPath">your kubeconfig file</span></span>
                        </div>
                    </div>
                </div>
            </div>

            <!-- Loading Overlay -->
            <div class="loading-overlay" id="loadingOverlay">
                <div class="loading-spinner"></div>
            </div>
        </div>
    </div>

    <!-- Status Bar -->
    <div class="status-bar">
        <div class="status-bar-item">
            <span class="status-dot" id="connectionStatus"></span>
            <span id="connectionText">Not connected</span>
        </div>
        <div class="status-bar-item" id="selectionStatus">
            0 selected
        </div>
    </div>

    <!-- Add/Edit Connection Modal -->
    <div class="modal-overlay" id="addConnectionModal">
        <div class="modal">
            <div class="modal-header">
                <span class="modal-title" id="modalTitle">Add Connection</span>
                <button class="modal-close" onclick="hideAddConnectionModal()">&times;</button>
            </div>
            <div class="modal-body">
                <div class="notice modal-notice hidden" id="lockedHint">
                    <div class="notice-body">This connection could not be decrypted (the encryption key may have changed). Re-enter its credentials and save to unlock it.</div>
                </div>
                <div class="form-group">
                    <label>Connection Name</label>
                    <input type="text" id="newProfileName" placeholder="My Cluster">
                </div>

                <div class="type-tabs">
                    <button class="type-tab active" onclick="switchProfileType('rancher', this)">Rancher</button>
                    <button class="type-tab" onclick="switchProfileType('eks', this)">AWS EKS</button>
                    <button class="type-tab" onclick="switchProfileType('static', this)">Manual</button>
                    <button class="type-tab" onclick="switchProfileType('oke', this)">Oracle OKE</button>
                </div>

                <!-- Rancher Panel -->
                <div class="type-panel active" id="panelRancher">
                    <div class="form-group">
                        <label>Rancher URL</label>
                        <input type="url" id="newRancherUrl" placeholder="https://rancher.example.com">
                    </div>
                    <div class="form-group">
                        <label>API Token</label>
                        <input type="password" id="newRancherToken" placeholder="token-xxxxx:yyyyyyy" autocomplete="off">
                    </div>
                    <div class="form-section-title">Or use username/password</div>
                    <div class="form-group">
                        <label>Username</label>
                        <input type="text" id="newRancherUsername" placeholder="admin">
                    </div>
                    <div class="form-group">
                        <label>Password</label>
                        <input type="password" id="newRancherPassword" placeholder="Password" autocomplete="off">
                    </div>
                    <div class="form-group">
                        <label>CA Certificate Path (optional)</label>
                        <input type="text" id="newRancherCACert" placeholder="/path/to/ca.pem">
                    </div>
                    <div class="checkbox-row">
                        <input type="checkbox" id="newRancherSkipTls">
                        <label for="newRancherSkipTls">Skip TLS verification</label>
                    </div>
                </div>

                <!-- EKS Panel -->
                <div class="type-panel" id="panelEks">
                    <div class="form-group">
                        <label>AWS Region</label>
                        <select id="newEksRegion">
                            <option value="">Select region...</option>
                        </select>
                    </div>
                    <div class="form-group">
                        <label>AWS Profile (from ~/.aws/credentials)</label>
                        <select id="newEksProfile">
                            <option value="">Select profile or enter credentials below...</option>
                        </select>
                    </div>
                    <div class="form-section-title">Or use direct credentials</div>
                    <div class="form-group">
                        <label>Access Key ID</label>
                        <input type="text" id="newEksAccessKey" placeholder="AKIA...">
                    </div>
                    <div class="form-group">
                        <label>Secret Access Key</label>
                        <input type="password" id="newEksSecretKey" placeholder="Secret key" autocomplete="off">
                    </div>
                    <div class="form-group">
                        <label>Session Token (optional)</label>
                        <input type="password" id="newEksSessionToken" placeholder="For temporary credentials" autocomplete="off">
                    </div>
                </div>

                <!-- Static Panel -->
                <div class="type-panel" id="panelStatic">
                    <div class="form-group">
                        <label>Kubeconfig Content</label>
                        <div style="display: flex; gap: 8px; margin-bottom: 8px;">
                            <button type="button" class="toolbar-btn toolbar-btn-secondary" onclick="loadKubeconfigFromFile()" style="flex: 1; justify-content: center;">
                                📂 Load from File
                            </button>
                        </div>
                        <input type="file" id="staticFileInput" class="hidden" onchange="onStaticFileChosen(this)">
                        <textarea id="newStaticKubeconfig" placeholder="Paste your kubeconfig YAML here or click 'Load from File' above..."></textarea>
                    </div>
                </div>

                <!-- Oracle OKE Panel -->
                <div class="type-panel" id="panelOke">
                    <div class="form-group">
                        <label for="newOciProfile">OCI Profile (from ~/.oci/config)</label>
                        <select id="newOciProfile" onchange="onOciProfileChange()">
                            <option value="">DEFAULT</option>
                            <option value="__manual__">(enter manually)</option>
                        </select>
                        <input type="text" id="newOciProfileManual" class="hidden" placeholder="Profile name (empty = DEFAULT)" style="margin-top: 6px;">
                    </div>
                    <div class="form-group">
                        <label for="newOciConfigFile">OCI Config File (optional)</label>
                        <input type="text" id="newOciConfigFile" placeholder="~/.oci/config">
                    </div>
                    <div class="form-group">
                        <label for="newOciRegion">Region</label>
                        <select id="newOciRegion">
                            <option value="">Use profile default</option>
                        </select>
                    </div>
                    <div class="form-group">
                        <label for="newOciCompartment">Compartment OCID (optional)</label>
                        <input type="text" id="newOciCompartment" placeholder="ocid1.compartment.oc1..">
                        <span class="option-hint">Empty = all compartments in the tenancy</span>
                    </div>
                    <div class="form-group">
                        <label for="newOciAuth">Authentication</label>
                        <select id="newOciAuth">
                            <option value="api_key">API key (default)</option>
                            <option value="security_token">Security token session (oci session authenticate)</option>
                        </select>
                    </div>
                    <div class="form-group">
                        <label for="newOkeEndpoint">Kubeconfig Endpoint</label>
                        <select id="newOkeEndpoint">
                            <option value="public">Public endpoint (default)</option>
                            <option value="private">Private endpoint (inside the VCN)</option>
                            <option value="vcn_hostname">VCN hostname (private DNS name)</option>
                        </select>
                    </div>
                    <span class="option-hint">No secrets are stored: credentials come from the OCI config profile, and the OCI CLI (oci) must be installed for kubectl authentication.</span>
                </div>
            </div>
            <div class="modal-footer">
                <button class="toolbar-btn toolbar-btn-secondary" onclick="hideAddConnectionModal()">Cancel</button>
                <button class="toolbar-btn" id="saveProfileBtn" onclick="saveNewProfile()">Save Connection</button>
            </div>
        </div>
    </div>

    <!-- Manage Kubeconfig Modal -->
    <div class="modal-overlay" id="manageKubeconfigModal">
        <div class="modal" style="max-width: 700px;">
            <div class="modal-header">
                <span class="modal-title">Manage Kubeconfig</span>
                <button class="modal-close" onclick="hideManageKubeconfigModal()">&times;</button>
            </div>
            <div class="modal-body">
                <div class="form-group">
                    <label>Kubeconfig Path</label>
                    <input type="text" id="kubeconfigPath" readonly style="background: var(--bg-tertiary); cursor: default;">
                </div>
                <div class="form-group">
                    <label>Contexts</label>
                    <div id="contextList" style="border: 1px solid var(--border); border-radius: 4px;">
                        <div style="padding: 20px; text-align: center; color: var(--text-secondary);">Loading contexts...</div>
                    </div>
                </div>
            </div>
            <div class="modal-footer">
                <button class="toolbar-btn toolbar-btn-secondary" onclick="loadContexts()">↻ Refresh</button>
                <button class="toolbar-btn toolbar-btn-secondary" onclick="hideManageKubeconfigModal()">Close</button>
            </div>
        </div>
    </div>

    <!-- Rename Context Modal -->
    <div class="modal-overlay" id="renameContextModal">
        <div class="modal" style="max-width: 400px;">
            <div class="modal-header">
                <span class="modal-title">Rename Context</span>
                <button class="modal-close" onclick="hideRenameContextModal()">&times;</button>
            </div>
            <div class="modal-body">
                <div class="form-group">
                    <label>Current Name</label>
                    <input type="text" id="renameOldName" readonly style="background: var(--bg-tertiary); cursor: default;">
                </div>
                <div class="form-group">
                    <label>New Name</label>
                    <input type="text" id="renameNewName" placeholder="Enter new context name">
                </div>
            </div>
            <div class="modal-footer">
                <button class="toolbar-btn toolbar-btn-secondary" onclick="hideRenameContextModal()">Cancel</button>
                <button class="toolbar-btn" onclick="confirmRenameContext()">Rename</button>
            </div>
        </div>
    </div>

    <!-- Edit Alias Modal -->
    <div class="modal-overlay" id="editAliasModal">
        <div class="modal" style="max-width: 400px;">
            <div class="modal-header">
                <span class="modal-title">Rename Cluster</span>
                <button class="modal-close" onclick="hideEditAliasModal()">&times;</button>
            </div>
            <div class="modal-body">
                <div class="form-group">
                    <label>Source Name</label>
                    <input type="text" id="aliasClusterName" readonly style="background: var(--bg-tertiary); cursor: default; font-family: monospace; font-size: 12px;">
                    <small style="color: var(--text-secondary); display: block; margin-top: 4px;">Original name from provider (cannot be changed)</small>
                </div>
                <div class="form-group">
                    <label>Display Name</label>
                    <input type="text" id="aliasNewName" placeholder="Enter a friendly name">
                    <small style="color: var(--text-secondary); display: block; margin-top: 4px;">This name will be used in generated kubeconfigs</small>
                </div>
                <input type="hidden" id="aliasClusterId">
                <input type="hidden" id="aliasProfileId">
            </div>
            <div class="modal-footer">
                <button class="toolbar-btn toolbar-btn-secondary" onclick="hideEditAliasModal()">Cancel</button>
                <button class="toolbar-btn" onclick="saveClusterAlias()">Save</button>
            </div>
        </div>
    </div>

    <!-- Toast -->
    <div class="toast" id="toast"></div>

    <script>
        let clusters = [];
        let editingAliasIndex = -1;
        let isConnected = false;
        let profiles = [];
        let selectedProfileId = null;
        // The source whose clusters are in the table ('__all__' for All
        // Sources). Set only after a successful load; Generate uses it.
        let loadedProfileId = null;
        // Incremented per cluster load so a slow, superseded response is ignored.
        let loadSeq = 0;
        let currentProfileType = 'rancher';
        let editingProfileId = null;
        let savingProfile = false;
        let storeAvailable = true;
        let kubeconfigPath = '';
        // Selection survives re-renders: keys of checked clusters, and keys of
        // clusters already shown once (only unseen active clusters start checked).
        let selectedKeys = new Set();
        let seenKeys = new Set();
        let toastTimer = null;

        const HTML_ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };

        // Default placeholders of the secret inputs; the edit form replaces them
        // with "(unchanged)" when a value is stored.
        const SECRET_PLACEHOLDERS = {
            newRancherToken: 'token-xxxxx:yyyyyyy',
            newRancherPassword: 'Password',
            newEksSecretKey: 'Secret key',
            newEksSessionToken: 'For temporary credentials',
            newStaticKubeconfig: "Paste your kubeconfig YAML here or click 'Load from File' above..."
        };

        // Get the auth token from meta tag
        function getAuthToken() {
            const meta = document.querySelector('meta[name="auth-token"]');
            return meta ? meta.getAttribute('content') : '';
        }

        // Authenticated fetch wrapper
        async function authFetch(url, options = {}) {
            const token = getAuthToken();
            const headers = options.headers || {};
            if (token) {
                headers['X-Auth-Token'] = token;
            }
            return fetch(url, { ...options, headers });
        }

        // Parses a response body as JSON. A non-JSON body (e.g. a plain-text
        // 401/403 from the security middleware) becomes {success:false, error}.
        async function readJSON(response) {
            const text = await response.text();
            try {
                const data = JSON.parse(text);
                if (data && typeof data === 'object') {
                    if (!response.ok) data.success = false;
                    return data;
                }
            } catch (e) {
                // fall through
            }
            return { success: false, error: text.trim() || ('HTTP ' + response.status) };
        }

        function errorOf(result, response) {
            return (result && result.error) || ('HTTP ' + response.status);
        }

        function escapeHtml(text) {
            return String(text == null ? '' : text).replace(/[&<>"']/g, ch => HTML_ESCAPES[ch]);
        }

        function showToast(message, type = 'success', duration) {
            const toast = document.getElementById('toast');
            toast.textContent = message;
            toast.className = 'toast ' + type + ' visible';
            clearTimeout(toastTimer);
            toastTimer = setTimeout(() => toast.classList.remove('visible'), duration || (type === 'success' ? 3000 : 6000));
        }

        function showNotice(noticeId, listId, items) {
            const notice = document.getElementById(noticeId);
            const list = document.getElementById(listId);
            list.innerHTML = '';
            (items || []).forEach(item => {
                const li = document.createElement('li');
                li.textContent = item;
                list.appendChild(li);
            });
            notice.classList.toggle('hidden', !items || items.length === 0);
        }

        function hideNotice(noticeId) {
            document.getElementById(noticeId).classList.add('hidden');
        }

        function setLoading(loading) {
            document.getElementById('loadingOverlay').classList.toggle('visible', loading);
        }

        // state is 'error', 'warning' or undefined.
        function updateConnectionStatus(connected, text, state) {
            isConnected = connected;
            const dot = document.getElementById('connectionStatus');
            dot.classList.toggle('connected', connected && state !== 'warning');
            dot.classList.toggle('warning', state === 'warning');
            dot.classList.toggle('error', state === 'error');
            document.getElementById('connectionText').textContent = text;
            document.getElementById('refreshBtn').disabled = !connected;
        }

        function updateSelectionStatus() {
            const selected = getSelectedClusters();
            document.getElementById('selectionStatus').textContent = selected.length + ' selected';
            document.getElementById('generateBtn').disabled = selected.length === 0;
        }

        function isActiveState(state) {
            const s = (state || '').toLowerCase();
            return s === 'active' || s === 'running' || s === 'ready' || s === 'static';
        }

        function getStatusClass(state) {
            const s = (state || '').toLowerCase();
            if (s === 'active' || s === 'running' || s === 'ready' || s === 'static') return 'status-active';
            if (s === 'error' || s === 'unavailable' || s === 'failed') return 'status-error';
            return 'status-inactive';
        }

        function clusterKey(cluster) {
            return (cluster.profileId || '') + '/' + cluster.id;
        }

        function resetSelection() {
            selectedKeys = new Set();
            seenKeys = new Set();
        }

        function clearClusters() {
            clusters = [];
            loadedProfileId = null;
            resetSelection();
            renderClusterTable();
        }

        function renderClusterTable() {
            const tbody = document.getElementById('clusterTableBody');
            const table = document.getElementById('clusterTable');
            const emptyState = document.getElementById('emptyState');

            tbody.innerHTML = '';

            clusters.forEach(cluster => {
                const key = clusterKey(cluster);
                if (!seenKeys.has(key)) {
                    seenKeys.add(key);
                    if (isActiveState(cluster.state)) selectedKeys.add(key);
                }
            });

            if (clusters.length === 0) {
                table.classList.add('hidden');
                emptyState.classList.remove('hidden');
                document.getElementById('clusterCount').textContent = '0 clusters';
                updateSelectAll();
                updateSelectionStatus();
                updatePrefixPreview();
                return;
            }

            table.classList.remove('hidden');
            emptyState.classList.add('hidden');
            document.getElementById('clusterCount').textContent = clusters.length + ' cluster' + (clusters.length !== 1 ? 's' : '');

            clusters.forEach((cluster, index) => {
                const tr = document.createElement('tr');
                const isActive = isActiveState(cluster.state);
                const isSelected = isActive && selectedKeys.has(clusterKey(cluster));
                // Display alias if set, otherwise fall back to the original name
                const displayName = cluster.alias || cluster.name;
                const localBadge = cluster.inLocalKubeconfig ? '<span class="local-config-badge">local</span>' : '';
                tr.innerHTML = ` + "`" + `
                    <td><input type="checkbox" id="cluster-${index}" ${isSelected ? 'checked' : ''} ${!isActive ? 'disabled' : ''} onchange="onClusterSelect(${index})"></td>
                    <td class="cluster-name-cell">
                        <div class="cluster-name-row">
                            <span class="cluster-display-name">${escapeHtml(displayName)}</span>
                            ${localBadge}
                            <button class="edit-alias-btn" onclick="showEditAliasModal(${index})" title="Edit name">✏️</button>
                        </div>
                        <div class="cluster-source-name">${escapeHtml(cluster.name)}</div>
                    </td>
                    <td><span class="cluster-source">${escapeHtml(cluster.profileName || '—')}</span></td>
                    <td>${escapeHtml(cluster.provider || '—')}</td>
                    <td><span class="status-badge ${getStatusClass(cluster.state)}">${escapeHtml(cluster.state)}</span></td>
                ` + "`" + `;
                if (isSelected) tr.classList.add('selected');
                tbody.appendChild(tr);
            });

            updateSelectAll();
            updateSelectionStatus();
            updatePrefixPreview();
        }

        function onClusterSelect(index) {
            const checkbox = document.getElementById('cluster-' + index);
            const cluster = clusters[index];
            if (!checkbox || !cluster) return;
            if (checkbox.checked) {
                selectedKeys.add(clusterKey(cluster));
            } else {
                selectedKeys.delete(clusterKey(cluster));
            }
            checkbox.closest('tr').classList.toggle('selected', checkbox.checked);
            updateSelectAll();
            updateSelectionStatus();
            updatePrefixPreview();
        }

        function toggleSelectAll() {
            const selectAll = document.getElementById('selectAll').checked;
            clusters.forEach((cluster, index) => {
                if (!isActiveState(cluster.state)) return;
                if (selectAll) {
                    selectedKeys.add(clusterKey(cluster));
                } else {
                    selectedKeys.delete(clusterKey(cluster));
                }
                const checkbox = document.getElementById('cluster-' + index);
                if (checkbox) {
                    checkbox.checked = selectAll;
                    checkbox.closest('tr').classList.toggle('selected', selectAll);
                }
            });
            updateSelectionStatus();
            updatePrefixPreview();
        }

        function updateSelectAll() {
            const activeClusters = clusters.filter(c => isActiveState(c.state));
            document.getElementById('selectAll').checked = activeClusters.length > 0 &&
                activeClusters.every(c => selectedKeys.has(clusterKey(c)));
        }

        function getSelectedClusters() {
            return clusters.filter(c => isActiveState(c.state) && selectedKeys.has(clusterKey(c)));
        }

        function toggleOptionsPanel() {
            const panel = document.getElementById('optionsPanel');
            panel.classList.toggle('collapsed');
        }

        function getGenerationOptions() {
            const prefix = document.getElementById('clusterPrefix').value.trim();
            const tagsInput = document.getElementById('aptakubeTags').value.trim();
            const separateFiles = document.getElementById('separateFiles').checked;
            const useSourcePrefix = document.getElementById('useSourcePrefix').checked;
            const mergeToKubeconfig = document.getElementById('mergeToKubeconfig').checked;

            const tags = tagsInput ? tagsInput.split(',').map(t => t.trim()).filter(t => t) : [];

            return { prefix, tags, separateFiles, useSourcePrefix, mergeToKubeconfig };
        }

        function setKubeconfigPath(path) {
            kubeconfigPath = path || '';
            document.getElementById('mergeTargetPath').textContent = kubeconfigPath || 'your kubeconfig file';
            document.getElementById('kubeconfigPath').value = kubeconfigPath;
        }

        function kubeconfigLabel() {
            return kubeconfigPath || 'your kubeconfig';
        }

        async function generateKubeconfig() {
            if (!selectedProfileId || loadedProfileId !== selectedProfileId) {
                showToast('Load the clusters of a connection first', 'error');
                return;
            }

            const selectedClusters = getSelectedClusters();
            if (selectedClusters.length === 0) {
                showToast('Please select at least one cluster', 'error');
                return;
            }

            const options = getGenerationOptions();

            // Cannot merge with separate files option
            if (options.mergeToKubeconfig && options.separateFiles) {
                showToast('Cannot merge when "Generate one file per cluster (zip)" is enabled', 'error');
                return;
            }

            const btn = document.getElementById('generateBtn');
            btn.disabled = true;
            btn.textContent = options.mergeToKubeconfig ? '⏳ Merging...' : '⏳ Generating...';

            const allSources = loadedProfileId === '__all__';
            const body = {
                selectedClusters: selectedClusters.map(c => ({ id: c.id, name: c.name, alias: c.alias, profileId: c.profileId })),
                clusterPrefix: options.prefix,
                aptakubeTags: options.tags,
                separateFiles: options.separateFiles,
                useSourcePrefix: options.useSourcePrefix,
                mergeToKubeconfig: options.mergeToKubeconfig
            };
            if (!allSources) {
                body.profileId = loadedProfileId;
            }

            try {
                const response = await authFetch(allSources ? '/api/generate/all' : '/api/generate/profile', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(body)
                });
                if (options.mergeToKubeconfig) {
                    await handleMergeResponse(response);
                } else {
                    await handleDownloadResponse(response);
                }
            } catch (error) {
                showToast('Failed to generate: ' + error.message, 'error');
            } finally {
                btn.textContent = '⬇ Generate Kubeconfig';
                updateSelectionStatus();
            }
        }

        async function handleMergeResponse(response) {
            const result = await readJSON(response);
            if (!result.success) {
                let msg = 'Failed to merge: ' + errorOf(result, response);
                showToast(msg, 'error', 10000);
                return;
            }
            const data = result.data || {};
            const added = data.added || [];
            const replaced = data.replaced || [];
            const failed = data.failed || [];

            // Refresh first so the "local" badges update; the merge result is
            // shown afterwards so a refresh error cannot hide it.
            await fetchClusters();

            let msg = 'Merged into ' + kubeconfigLabel() + ': ' + added.length + ' added, ' + replaced.length + ' replaced';
            if (failed.length > 0) {
                msg += ', ' + failed.length + ' failed:\n' + failed.map(f => f.cluster + ': ' + f.error).join('\n');
                showToast(msg, 'warning', 10000);
            } else {
                showToast(msg, 'success', 4000);
            }
        }

        function parseWarningsHeader(value) {
            if (!value) return [];
            try {
                const list = JSON.parse(value);
                return Array.isArray(list) ? list.map(String) : [String(list)];
            } catch (e) {
                return [value];
            }
        }

        async function handleDownloadResponse(response) {
            const contentType = response.headers.get('Content-Type') || '';
            if (!response.ok || contentType.includes('application/json')) {
                const result = await readJSON(response);
                showToast('Generation failed: ' + errorOf(result, response), 'error', 10000);
                return;
            }

            const warnings = parseWarningsHeader(response.headers.get('X-Kubeconfig-Warnings'));
            const blob = await response.blob();
            const url = window.URL.createObjectURL(blob);
            const a = document.createElement('a');
            a.href = url;
            a.download = contentType.includes('application/zip') ? 'kubeconfigs.zip' : 'kubeconfig.yaml';
            document.body.appendChild(a);
            a.click();
            a.remove();
            setTimeout(() => window.URL.revokeObjectURL(url), 1000);

            if (warnings.length > 0) {
                showToast('Downloaded, but ' + warnings.length + ' cluster(s) are missing:\n' + warnings.join('\n'), 'warning', 10000);
            } else {
                showToast('Kubeconfig downloaded successfully!', 'success');
            }
        }

        // Status / Profile Management Functions
        async function loadStatus() {
            try {
                const response = await authFetch('/api/status');
                const result = await readJSON(response);
                if (!result.success || !result.data) {
                    console.error('Failed to load status:', result.error);
                    return;
                }
                const status = result.data;
                setKubeconfigPath(status.kubeconfigPath);
                setStoreAvailable(status.storeAvailable !== false, status.storeError);
                showNotice('loadWarningsBanner', 'loadWarningsList', status.loadWarnings || []);
            } catch (error) {
                console.error('Failed to load status:', error);
            }
        }

        function setStoreAvailable(available, error) {
            storeAvailable = available;
            const addBtn = document.getElementById('addConnectionBtn');
            addBtn.disabled = !available;
            addBtn.title = available ? 'Add Connection' : 'Saved connections are unavailable';
            const el = document.getElementById('storeError');
            el.textContent = available ? '' : 'Saved connections are unavailable: ' + (error || 'the profile store could not be initialized');
            el.classList.toggle('hidden', available);
        }

        async function loadProfiles() {
            try {
                const response = await authFetch('/api/profiles');
                const result = await readJSON(response);
                if (!result.success) {
                    profiles = [];
                    renderProfileList(errorOf(result, response));
                    return;
                }
                profiles = (result.data || []).slice().sort((a, b) =>
                    (a.name || '').localeCompare(b.name || '') || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
                renderProfileList();
            } catch (error) {
                console.error('Failed to load profiles:', error);
                renderProfileList(error.message);
            }
        }

        function renderProfileList(errorMessage) {
            const list = document.getElementById('profileList');
            if (errorMessage) {
                // When the store is unavailable the sidebar notice already says why.
                list.innerHTML = storeAvailable
                    ? '<div class="empty-profiles">Could not load connections: ' + escapeHtml(errorMessage) + '</div>'
                    : '';
                return;
            }
            if (profiles.length === 0) {
                list.innerHTML = storeAvailable ? '<div class="empty-profiles">No saved connections. Click + to add one.</div>' : '';
                return;
            }

            // Add "All Sources" option at the top
            const allSourcesActive = selectedProfileId === '__all__' ? 'active' : '';
            let html = ` + "`" + `
                <div class="profile-item ${allSourcesActive}" data-action="all">
                    <span class="profile-icon">🌐</span>
                    <div class="profile-info">
                        <div class="profile-name">All Sources</div>
                        <div class="profile-type">${profiles.length} connection${profiles.length !== 1 ? 's' : ''}</div>
                    </div>
                </div>
                <div class="profile-separator"></div>
            ` + "`" + `;

            html += profiles.map((p, i) => {
                const classes = ['profile-item'];
                if (p.id === selectedProfileId) classes.push('active');
                if (p.locked) classes.push('locked');
                const lock = p.locked ? '<span class="profile-lock" title="Could not be decrypted">🔒</span>' : '';
                const typeText = escapeHtml(p.type) + (p.locked ? ' · locked, edit to re-enter credentials' : '');
                return ` + "`" + `
                    <div class="${classes.join(' ')}" data-action="select" data-idx="${i}">
                        <span class="profile-icon">${getProfileIcon(p.type)}</span>
                        <div class="profile-info">
                            <div class="profile-name">${lock}${escapeHtml(p.name)}</div>
                            <div class="profile-type">${typeText}</div>
                        </div>
                        <div class="profile-actions">
                            <button class="profile-action-btn" data-action="edit" data-idx="${i}" title="Edit">✎</button>
                            <button class="profile-action-btn delete" data-action="delete" data-idx="${i}" title="Delete">&times;</button>
                        </div>
                    </div>
                ` + "`" + `;
            }).join('');

            list.innerHTML = html;
        }

        function onProfileListClick(event) {
            const el = event.target.closest('[data-action]');
            if (!el) return;
            const action = el.dataset.action;
            if (action === 'all') {
                selectAllSources();
                return;
            }
            const p = profiles[Number(el.dataset.idx)];
            if (!p) return;
            if (action === 'edit') {
                editProfile(p.id);
            } else if (action === 'delete') {
                deleteProfile(p.id);
            } else if (action === 'select') {
                selectProfile(p.id);
            }
        }

        function getProfileIcon(type) {
            switch (type) {
                case 'rancher': return '🐮';
                case 'eks': return '☁';
                case 'static': return '📄';
                case 'oke': return '🟥';
                default: return '📁';
            }
        }

        // loadClusters fetches the clusters of sourceId (a profile id or
        // '__all__') and renders them. describe(count, failed) builds the status text.
        async function loadClusters(sourceId, url, fetchOptions, timeoutMs, describe) {
            const seq = ++loadSeq;
            hideNotice('clusterWarningsNotice');
            setLoading(true);
            updateConnectionStatus(false, 'Loading clusters...');

            const controller = new AbortController();
            const timeoutId = setTimeout(() => controller.abort(), timeoutMs);

            try {
                const response = await authFetch(url, { ...fetchOptions, signal: controller.signal });
                const result = await readJSON(response);
                if (seq !== loadSeq) return;
                if (!result.success) {
                    throw new Error(errorOf(result, response));
                }
                if (loadedProfileId !== sourceId) {
                    resetSelection();
                }
                clusters = result.data || [];
                loadedProfileId = sourceId;
                renderClusterTable();

                const warnings = (result.warnings && result.warnings.length > 0)
                    ? result.warnings
                    : (result.error ? [result.error] : []);
                if (warnings.length > 0) {
                    showNotice('clusterWarningsNotice', 'clusterWarningsList', warnings);
                    showToast('Some sources failed to load:\n' + warnings.join('\n'), 'warning', 10000);
                    updateConnectionStatus(true, describe(clusters.length, true), 'warning');
                } else {
                    updateConnectionStatus(true, describe(clusters.length, false));
                }
            } catch (error) {
                if (seq !== loadSeq) return;
                console.error('Failed to load clusters:', error);
                clearClusters();
                if (error.name === 'AbortError') {
                    showToast('Request timed out - check your connection settings', 'error');
                } else {
                    showToast('Failed to load clusters: ' + error.message, 'error');
                }
                updateConnectionStatus(false, 'Connection failed', 'error');
            } finally {
                clearTimeout(timeoutId);
                if (seq === loadSeq) setLoading(false);
            }
        }

        async function selectProfile(profileId) {
            const profile = profiles.find(p => p.id === profileId);
            if (!profile) return;
            if (profile.locked) {
                // A locked profile has no usable credentials until re-entered.
                editProfile(profileId);
                return;
            }

            selectedProfileId = profileId;
            renderProfileList();

            await loadClusters(profileId, '/api/clusters/profile', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ profileId })
            }, 30000, () => 'Connected to ' + profile.name);
        }

        async function selectAllSources() {
            selectedProfileId = '__all__';
            renderProfileList();

            if (profiles.length === 0) {
                clearClusters();
                updateConnectionStatus(false, 'Not connected');
                showToast('No connections configured', 'error');
                return;
            }

            await loadClusters('__all__', '/api/clusters/all', { method: 'GET' }, 60000,
                (count, failed) => 'All Sources (' + count + ' clusters' + (failed ? ', some sources failed' : '') + ')');
        }

        function fetchClusters() {
            // Refresh clusters based on current selection
            if (selectedProfileId === '__all__') {
                return selectAllSources();
            } else if (selectedProfileId) {
                return selectProfile(selectedProfileId);
            }
            return Promise.resolve();
        }

        // Mirrors sanitizeSourceName in server.go.
        function sanitizeSourceName(name) {
            return (name || '').toLowerCase()
                .replace(/[ _]/g, '-')
                .replace(/[^a-z0-9-]/g, '')
                .replace(/-+/g, '-')
                .replace(/^-+|-+$/g, '');
        }

        function sourcePrefixFor(cluster) {
            let name = cluster.profileName;
            if (!name) {
                const p = profiles.find(p => p.id === cluster.profileId);
                name = p ? p.name : '';
            }
            const sanitized = sanitizeSourceName(name);
            return sanitized ? sanitized + '-' : '';
        }

        function updatePrefixPreview() {
            const prefix = document.getElementById('clusterPrefix').value.trim();
            const useSource = document.getElementById('useSourcePrefix').checked;
            const previewEl = document.getElementById('prefixPreview');

            if (!prefix && !useSource) {
                previewEl.classList.remove('visible');
                previewEl.innerHTML = '';
                return;
            }

            // The generator writes prefix + source prefix + (alias or name).
            const selected = getSelectedClusters();
            const exampleClusters = selected.length > 0 ? selected : clusters;
            const highlight = text => '<span class="prefix-highlight">' + escapeHtml(text) + '</span>';

            if (exampleClusters.length === 0) {
                previewEl.innerHTML = 'Preview: ' + highlight(prefix + (useSource ? 'source-' : '')) + 'example-cluster';
            } else {
                const examples = exampleClusters.slice(0, 2).map(c =>
                    highlight(prefix + (useSource ? sourcePrefixFor(c) : '')) + escapeHtml(c.alias || c.name)
                );
                previewEl.innerHTML = 'Preview: ' + examples.join(', ') + (exampleClusters.length > 2 ? ', ...' : '');
            }

            previewEl.classList.add('visible');
        }

        async function deleteProfile(profileId) {
            const profile = profiles.find(p => p.id === profileId);
            const label = profile ? '"' + profile.name + '"' : 'this connection';
            if (!confirm('Are you sure you want to delete ' + label + '?')) return;

            try {
                const response = await authFetch('/api/profiles/' + encodeURIComponent(profileId), {
                    method: 'DELETE'
                });
                const result = await readJSON(response);
                if (!result.success) {
                    throw new Error(errorOf(result, response));
                }
                showToast('Connection deleted', 'success');
                await loadProfiles();
                loadStatus();
                if (selectedProfileId === profileId || (selectedProfileId === '__all__' && profiles.length === 0)) {
                    selectedProfileId = null;
                    clearClusters();
                    updateConnectionStatus(false, 'Not connected');
                    renderProfileList();
                } else if (selectedProfileId === '__all__') {
                    // Reload so the deleted connection's clusters leave the table.
                    await selectAllSources();
                }
            } catch (error) {
                showToast('Failed to delete: ' + error.message, 'error');
            }
        }

        function showAddConnectionModal() {
            if (!storeAvailable) {
                showToast('Saved connections are unavailable', 'error');
                return;
            }
            editingProfileId = null;
            document.getElementById('modalTitle').textContent = 'Add Connection';
            document.getElementById('addConnectionModal').classList.add('show');
            loadAWSData();
            loadOCIData();
        }

        function resetSecretPlaceholders() {
            Object.keys(SECRET_PLACEHOLDERS).forEach(id => {
                document.getElementById(id).placeholder = SECRET_PLACEHOLDERS[id];
            });
        }

        function hideAddConnectionModal() {
            document.getElementById('addConnectionModal').classList.remove('show');
            editingProfileId = null;
            // Reset form
            ['newProfileName', 'newRancherUrl', 'newRancherToken', 'newRancherUsername', 'newRancherPassword',
             'newRancherCACert', 'newEksRegion', 'newEksProfile', 'newEksAccessKey', 'newEksSecretKey',
             'newEksSessionToken', 'newStaticKubeconfig', 'newOciProfile', 'newOciProfileManual',
             'newOciConfigFile', 'newOciRegion', 'newOciCompartment'].forEach(id => {
                document.getElementById(id).value = '';
            });
            document.getElementById('newOciAuth').value = 'api_key';
            document.getElementById('newOkeEndpoint').value = 'public';
            onOciProfileChange();
            document.getElementById('newRancherSkipTls').checked = false;
            resetSecretPlaceholders();
            document.getElementById('lockedHint').classList.add('hidden');
            currentProfileType = 'rancher';
            document.querySelectorAll('.type-tab').forEach((t, i) => {
                t.classList.toggle('active', i === 0);
            });
            document.querySelectorAll('.type-panel').forEach((p, i) => {
                p.classList.toggle('active', i === 0);
            });
        }

        // Selects value in a <select>, adding it as an option when missing.
        function selectOption(select, value) {
            if (value && !Array.from(select.options).some(o => o.value === value)) {
                select.appendChild(new Option(value, value));
            }
            select.value = value || '';
        }

        function markUnchanged(id, hasValue, text) {
            document.getElementById(id).placeholder = hasValue ? (text || '(unchanged)') : SECRET_PLACEHOLDERS[id];
        }

        async function editProfile(profileId) {
            const profile = profiles.find(p => p.id === profileId);
            if (!profile) {
                showToast('Profile not found', 'error');
                return;
            }

            editingProfileId = profileId;
            document.getElementById('modalTitle').textContent = 'Edit Connection';
            document.getElementById('lockedHint').classList.toggle('hidden', !profile.locked);

            // Load the AWS and OCI lists first so the stored values can be selected
            await Promise.all([loadAWSData(), loadOCIData()]);

            // Fill in the form with profile data. Secrets are never sent to the
            // page: their inputs stay empty and show "(unchanged)" instead.
            document.getElementById('newProfileName').value = profile.name || '';

            // Set the correct type tab
            currentProfileType = profile.type;
            const typeMap = { 'rancher': 0, 'eks': 1, 'static': 2, 'oke': 3 };
            const typeIndex = typeMap[profile.type] || 0;
            document.querySelectorAll('.type-tab').forEach((t, i) => {
                t.classList.toggle('active', i === typeIndex);
            });
            document.querySelectorAll('.type-panel').forEach((p, i) => {
                p.classList.toggle('active', i === typeIndex);
            });

            resetSecretPlaceholders();
            switch (profile.type) {
                case 'rancher':
                    document.getElementById('newRancherUrl').value = profile.rancherUrl || '';
                    document.getElementById('newRancherUsername').value = profile.username || '';
                    document.getElementById('newRancherCACert').value = profile.caCert || '';
                    document.getElementById('newRancherSkipTls').checked = profile.skipTls || false;
                    markUnchanged('newRancherToken', profile.hasToken);
                    markUnchanged('newRancherPassword', profile.hasPassword);
                    break;
                case 'eks':
                    selectOption(document.getElementById('newEksRegion'), profile.awsRegion);
                    selectOption(document.getElementById('newEksProfile'), profile.awsProfile);
                    document.getElementById('newEksAccessKey').value = profile.accessKey || '';
                    markUnchanged('newEksSecretKey', profile.hasSecretKey);
                    markUnchanged('newEksSessionToken', profile.hasSessionToken);
                    break;
                case 'static':
                    markUnchanged('newStaticKubeconfig', profile.hasKubeconfig,
                        '(unchanged: ' + (profile.kubeconfigContexts || 0) + ' contexts stored)');
                    break;
                case 'oke':
                    setOciProfile(profile.ociProfile);
                    document.getElementById('newOciConfigFile').value = profile.ociConfigFile || '';
                    selectOption(document.getElementById('newOciRegion'), profile.ociRegion);
                    document.getElementById('newOciCompartment').value = profile.ociCompartment || '';
                    selectOption(document.getElementById('newOciAuth'), profile.ociAuth || 'api_key');
                    selectOption(document.getElementById('newOkeEndpoint'), profile.okeEndpoint || 'public');
                    break;
            }

            document.getElementById('addConnectionModal').classList.add('show');
        }

        function switchProfileType(type, btn) {
            currentProfileType = type;
            document.querySelectorAll('.type-tab').forEach(t => t.classList.remove('active'));
            btn.classList.add('active');

            document.querySelectorAll('.type-panel').forEach(p => p.classList.remove('active'));
            document.getElementById('panel' + type.charAt(0).toUpperCase() + type.slice(1)).classList.add('active');
        }

        async function loadAWSData() {
            const optionsHtml = items => items.map(v => '<option value="' + escapeHtml(v) + '">' + escapeHtml(v) + '</option>').join('');
            try {
                // Load AWS profiles
                const profilesRes = await authFetch('/api/aws/profiles');
                const profilesData = await readJSON(profilesRes);
                if (profilesData.success && profilesData.data) {
                    const select = document.getElementById('newEksProfile');
                    select.innerHTML = '<option value="">Select profile or enter credentials below...</option>' +
                        optionsHtml(profilesData.data);
                }

                // Load AWS regions
                const regionsRes = await authFetch('/api/aws/regions');
                const regionsData = await readJSON(regionsRes);
                if (regionsData.success && regionsData.data) {
                    const select = document.getElementById('newEksRegion');
                    select.innerHTML = '<option value="">Select region...</option>' + optionsHtml(regionsData.data);
                }
            } catch (error) {
                console.error('Failed to load AWS data:', error);
            }
        }

        // The OCI profile <select> lists the profiles of the OCI config file plus
        // "(enter manually)"; when the list is empty or unavailable only the
        // text input is shown.
        const OCI_MANUAL = '__manual__';

        function ociProfileListAvailable() {
            return !document.getElementById('newOciProfile').classList.contains('hidden');
        }

        function onOciProfileChange() {
            const manual = !ociProfileListAvailable() || document.getElementById('newOciProfile').value === OCI_MANUAL;
            document.getElementById('newOciProfileManual').classList.toggle('hidden', !manual);
        }

        function ociProfileValue() {
            const select = document.getElementById('newOciProfile');
            if (!ociProfileListAvailable() || select.value === OCI_MANUAL) {
                return inputValue('newOciProfileManual').trim();
            }
            return select.value.trim();
        }

        function setOciProfile(value) {
            const select = document.getElementById('newOciProfile');
            const manual = document.getElementById('newOciProfileManual');
            manual.value = '';
            if (!ociProfileListAvailable()) {
                manual.value = value || '';
            } else if (value && value !== OCI_MANUAL) {
                selectOption(select, value);
            } else {
                select.value = '';
            }
            onOciProfileChange();
        }

        function setOciProfileList(names) {
            const select = document.getElementById('newOciProfile');
            const previous = select.value;
            const options = names.filter(n => n && n !== 'DEFAULT' && n !== OCI_MANUAL)
                .map(n => '<option value="' + escapeHtml(n) + '">' + escapeHtml(n) + '</option>').join('');
            select.innerHTML = '<option value="">DEFAULT</option>' + options +
                '<option value="' + OCI_MANUAL + '">(enter manually)</option>';
            select.classList.toggle('hidden', names.length === 0);
            if (Array.from(select.options).some(o => o.value === previous)) select.value = previous;
            onOciProfileChange();
        }

        async function loadOCIData() {
            const optionsHtml = items => items.map(v => '<option value="' + escapeHtml(v) + '">' + escapeHtml(v) + '</option>').join('');
            // Profiles and regions load independently: one failing keeps the other.
            const profilesTask = (async () => {
                try {
                    const res = await authFetch('/api/oci/profiles');
                    const data = await readJSON(res);
                    if (!data.success) throw new Error(errorOf(data, res));
                    setOciProfileList(Array.isArray(data.data) ? data.data : []);
                } catch (error) {
                    console.error('Failed to load OCI profiles:', error);
                    setOciProfileList([]);
                }
            })();
            const regionsTask = (async () => {
                try {
                    const res = await authFetch('/api/oci/regions');
                    const data = await readJSON(res);
                    if (data.success && Array.isArray(data.data)) {
                        const select = document.getElementById('newOciRegion');
                        const previous = select.value;
                        select.innerHTML = '<option value="">Use profile default</option>' + optionsHtml(data.data);
                        if (Array.from(select.options).some(o => o.value === previous)) select.value = previous;
                    }
                } catch (error) {
                    console.error('Failed to load OCI regions:', error);
                }
            })();
            await Promise.all([profilesTask, regionsTask]);
        }

        function inputValue(id) {
            return document.getElementById(id).value;
        }

        async function saveNewProfile() {
            if (savingProfile) return;

            const name = inputValue('newProfileName').trim();
            if (!name) {
                showToast('Please enter a connection name', 'error');
                return;
            }

            const isEditing = editingProfileId !== null;
            const editedId = editingProfileId;
            const existing = isEditing ? profiles.find(p => p.id === editedId) : null;
            // Secrets already stored for this profile (only when the type is unchanged).
            const stored = existing && existing.type === currentProfileType ? existing : {};

            // Names, URLs, tokens and keys are trimmed (pasted values often carry
            // stray whitespace); only the password is sent exactly as typed.
            // Secrets are sent only when typed, so a blank field keeps the
            // stored value.
            const profileData = { name, type: currentProfileType };

            switch (currentProfileType) {
                case 'rancher': {
                    const token = inputValue('newRancherToken');
                    const password = inputValue('newRancherPassword');
                    const username = inputValue('newRancherUsername').trim();
                    profileData.rancherUrl = inputValue('newRancherUrl').trim();
                    profileData.username = username;
                    profileData.caCert = inputValue('newRancherCACert').trim();
                    profileData.skipTls = document.getElementById('newRancherSkipTls').checked;
                    if (token.trim() !== '') profileData.token = token.trim();
                    if (password.trim() !== '') profileData.password = password;
                    if (!profileData.rancherUrl) {
                        showToast('Please enter Rancher URL', 'error');
                        return;
                    }
                    // Mirrors the server's merge rules for blank secrets.
                    const hasToken = token.trim() !== '' || (stored.hasToken && username === '');
                    const hasUserPass = username !== '' &&
                        (password.trim() !== '' || (stored.hasPassword && username === stored.username));
                    if (!hasToken && !hasUserPass) {
                        showToast('Please enter API token or username/password', 'error');
                        return;
                    }
                    break;
                }
                case 'eks': {
                    const secretKey = inputValue('newEksSecretKey');
                    const sessionToken = inputValue('newEksSessionToken');
                    profileData.awsRegion = inputValue('newEksRegion');
                    profileData.awsProfile = inputValue('newEksProfile');
                    profileData.accessKey = inputValue('newEksAccessKey').trim();
                    if (secretKey.trim() !== '') profileData.secretKey = secretKey.trim();
                    if (sessionToken.trim() !== '') profileData.sessionToken = sessionToken.trim();
                    if (!profileData.awsRegion) {
                        showToast('Please select an AWS region', 'error');
                        return;
                    }
                    const hasSecret = secretKey.trim() !== '' ||
                        (stored.hasSecretKey && profileData.accessKey !== '' && profileData.accessKey === stored.accessKey);
                    if (!profileData.awsProfile && (!profileData.accessKey || !hasSecret)) {
                        showToast('Please select an AWS profile or enter credentials', 'error');
                        return;
                    }
                    break;
                }
                case 'oke': {
                    // OKE stores no secrets; every field is optional.
                    profileData.ociProfile = ociProfileValue();
                    profileData.ociConfigFile = inputValue('newOciConfigFile').trim();
                    profileData.ociRegion = inputValue('newOciRegion').trim();
                    profileData.ociCompartment = inputValue('newOciCompartment').trim();
                    profileData.ociAuth = inputValue('newOciAuth');
                    profileData.okeEndpoint = inputValue('newOkeEndpoint');
                    break;
                }
                case 'static': {
                    const kubeconfig = inputValue('newStaticKubeconfig');
                    if (kubeconfig.trim() !== '') {
                        profileData.kubeconfig = kubeconfig;
                    } else if (!stored.hasKubeconfig) {
                        showToast('Please enter kubeconfig content', 'error');
                        return;
                    }
                    break;
                }
            }

            const saveBtn = document.getElementById('saveProfileBtn');
            savingProfile = true;
            saveBtn.disabled = true;
            try {
                const url = isEditing ? '/api/profiles/' + encodeURIComponent(editedId) : '/api/profiles';
                const response = await authFetch(url, {
                    method: isEditing ? 'PUT' : 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(profileData)
                });
                const result = await readJSON(response);
                if (!result.success) {
                    throw new Error(errorOf(result, response));
                }
                showToast(isEditing ? 'Connection updated!' : 'Connection saved!', 'success');
                hideAddConnectionModal();
                await loadProfiles();
                loadStatus();
                // Refresh the table when it shows the saved connection.
                if (selectedProfileId === '__all__' || (isEditing && selectedProfileId === editedId)) {
                    fetchClusters();
                }
            } catch (error) {
                showToast('Failed to save: ' + error.message, 'error');
            } finally {
                savingProfile = false;
                saveBtn.disabled = false;
            }
        }

        function setStaticKubeconfig(content) {
            document.getElementById('newStaticKubeconfig').value = content;
            showToast('Kubeconfig loaded from file', 'success');
        }

        async function loadKubeconfigFromFile() {
            const api = window.electronAPI;
            if (api && typeof api.openKubeconfigFile === 'function') {
                try {
                    const result = await api.openKubeconfigFile();
                    if (!result || result.canceled) return;
                    if (result.error) {
                        showToast('Failed to read file: ' + result.error, 'error');
                        return;
                    }
                    setStaticKubeconfig(result.content || '');
                } catch (error) {
                    showToast('Failed to load file: ' + error.message, 'error');
                }
                return;
            }

            // Browser: use a file picker and read the file locally.
            const input = document.getElementById('staticFileInput');
            input.value = '';
            input.click();
        }

        function onStaticFileChosen(input) {
            const file = input.files && input.files[0];
            if (!file) return;
            const reader = new FileReader();
            reader.onload = () => setStaticKubeconfig(String(reader.result || ''));
            reader.onerror = () => showToast('Failed to read file: ' + (reader.error ? reader.error.message : 'unknown error'), 'error');
            reader.readAsText(file);
        }

        // ========== Kubeconfig Context Management ==========

        let kubeconfigContexts = [];

        function showManageKubeconfigModal() {
            document.getElementById('manageKubeconfigModal').classList.add('show');
            loadContexts();
        }

        function hideManageKubeconfigModal() {
            document.getElementById('manageKubeconfigModal').classList.remove('show');
        }

        function contextMessage(text, color) {
            return '<div style="padding: 20px; text-align: center; color: var(' + color + ');">' + escapeHtml(text) + '</div>';
        }

        async function loadContexts() {
            const contextList = document.getElementById('contextList');
            contextList.innerHTML = contextMessage('Loading contexts...', '--text-secondary');
            document.getElementById('kubeconfigPath').value = kubeconfigPath;

            try {
                // Get kubeconfig path
                const pathResponse = await authFetch('/api/context');
                const pathResult = await readJSON(pathResponse);
                if (pathResult.success && pathResult.data && pathResult.data.kubeconfigPath) {
                    setKubeconfigPath(pathResult.data.kubeconfigPath);
                }

                // Get contexts list
                const response = await authFetch('/api/contexts');
                const result = await readJSON(response);

                if (!result.success) {
                    contextList.innerHTML = contextMessage('Failed to load contexts: ' + errorOf(result, response), '--error');
                    return;
                }

                kubeconfigContexts = result.data || [];

                if (kubeconfigContexts.length === 0) {
                    contextList.innerHTML = contextMessage('No contexts found in kubeconfig', '--text-secondary');
                    return;
                }

                renderContextList();
            } catch (error) {
                contextList.innerHTML = contextMessage('Error: ' + error.message, '--error');
            }
        }

        function renderContextList() {
            const contextList = document.getElementById('contextList');
            const btnStyle = 'padding: 4px 8px; font-size: 11px;';
            contextList.innerHTML = kubeconfigContexts.map((ctx, i) => ` + "`" + `
                <div style="display: flex; align-items: center; padding: 10px 12px; border-bottom: 1px solid var(--border); gap: 10px;">
                    <div style="flex: 1; min-width: 0;">
                        <div style="display: flex; align-items: center; gap: 8px;">
                            <span style="font-weight: 500; color: var(--text-bright); word-break: break-all;">${escapeHtml(ctx.name)}</span>
                            ${ctx.isCurrent ? '<span style="background: var(--success); color: white; padding: 2px 6px; border-radius: 3px; font-size: 10px; font-weight: 600;">CURRENT</span>' : ''}
                        </div>
                        <div style="font-size: 11px; color: var(--text-secondary); margin-top: 4px;">
                            <span title="Cluster">📦 ${escapeHtml(ctx.cluster)}</span>
                            <span style="margin-left: 12px;" title="User">👤 ${escapeHtml(ctx.user)}</span>
                            ${ctx.namespace ? '<span style="margin-left: 12px;" title="Namespace">📁 ' + escapeHtml(ctx.namespace) + '</span>' : ''}
                        </div>
                    </div>
                    <div style="display: flex; gap: 4px; flex-shrink: 0;">
                        ${!ctx.isCurrent ? '<button data-action="use" data-idx="' + i + '" class="toolbar-btn toolbar-btn-secondary" style="' + btnStyle + '" title="Set as current">✓ Use</button>' : ''}
                        <button data-action="rename" data-idx="${i}" class="toolbar-btn toolbar-btn-secondary" style="${btnStyle}" title="Rename">✏️</button>
                        <button data-action="delete" data-idx="${i}" class="toolbar-btn toolbar-btn-secondary" style="${btnStyle} color: var(--error);" title="Delete">🗑️</button>
                    </div>
                </div>
            ` + "`" + `).join('');
        }

        function onContextListClick(event) {
            const el = event.target.closest('button[data-action]');
            if (!el) return;
            const ctx = kubeconfigContexts[Number(el.dataset.idx)];
            if (!ctx) return;
            switch (el.dataset.action) {
                case 'use': setCurrentContext(ctx.name); break;
                case 'rename': showRenameContextModal(ctx.name); break;
                case 'delete': deleteContext(ctx.name); break;
            }
        }

        async function setCurrentContext(name) {
            try {
                const response = await authFetch('/api/context', {
                    method: 'PUT',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ context: name })
                });
                const result = await readJSON(response);
                if (result.success) {
                    showToast('Switched to context: ' + name, 'success');
                    loadContexts();
                } else {
                    showToast('Failed to switch context: ' + errorOf(result, response), 'error');
                }
            } catch (error) {
                showToast('Error: ' + error.message, 'error');
            }
        }

        async function deleteContext(name) {
            if (!confirm('Delete context "' + name + '"?\n\nThis will also remove any orphaned cluster and user entries that are no longer used by other contexts.')) {
                return;
            }

            try {
                const response = await authFetch('/api/context/delete', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ name: name, cleanupOrphans: true })
                });
                const result = await readJSON(response);
                if (result.success) {
                    showToast('Context deleted: ' + name, 'success');
                    loadContexts();
                } else {
                    showToast('Failed to delete context: ' + errorOf(result, response), 'error');
                }
            } catch (error) {
                showToast('Error: ' + error.message, 'error');
            }
        }

        function showRenameContextModal(oldName) {
            document.getElementById('renameOldName').value = oldName;
            document.getElementById('renameNewName').value = '';
            document.getElementById('renameContextModal').classList.add('show');
            document.getElementById('renameNewName').focus();
        }

        function hideRenameContextModal() {
            document.getElementById('renameContextModal').classList.remove('show');
        }

        async function confirmRenameContext() {
            const oldName = document.getElementById('renameOldName').value;
            const newName = document.getElementById('renameNewName').value.trim();

            if (!newName) {
                showToast('Please enter a new name', 'error');
                return;
            }

            if (oldName === newName) {
                hideRenameContextModal();
                return;
            }

            try {
                const response = await authFetch('/api/context/rename', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ oldName: oldName, newName: newName })
                });
                const result = await readJSON(response);
                if (result.success) {
                    showToast('Context renamed: ' + oldName + ' → ' + newName, 'success');
                    hideRenameContextModal();
                    loadContexts();
                } else {
                    showToast('Failed to rename: ' + errorOf(result, response), 'error');
                }
            } catch (error) {
                showToast('Error: ' + error.message, 'error');
            }
        }

        // Cluster alias modal functions
        function showEditAliasModal(index) {
            if (index < 0 || index >= clusters.length) {
                showToast('Invalid cluster index', 'error');
                return;
            }

            const cluster = clusters[index];
            editingAliasIndex = index;

            document.getElementById('aliasClusterName').value = cluster.name;
            // Pre-populate with alias if set, otherwise use the original name
            document.getElementById('aliasNewName').value = cluster.alias || cluster.name;
            document.getElementById('aliasClusterId').value = cluster.id;
            document.getElementById('aliasProfileId').value = cluster.profileId;

            document.getElementById('editAliasModal').classList.add('show');
            const input = document.getElementById('aliasNewName');
            input.focus();
            input.select(); // Select all text so user can easily type over it
        }

        function hideEditAliasModal() {
            document.getElementById('editAliasModal').classList.remove('show');
            editingAliasIndex = -1;
        }

        async function saveClusterAlias() {
            const index = editingAliasIndex;
            const clusterId = document.getElementById('aliasClusterId').value;
            const profileId = document.getElementById('aliasProfileId').value;
            const sourceName = document.getElementById('aliasClusterName').value;
            let displayName = document.getElementById('aliasNewName').value.trim();

            if (!clusterId || !profileId) {
                showToast('Missing cluster or profile information', 'error');
                return;
            }

            // If display name matches source name, clear the alias (use default)
            const alias = (displayName === sourceName) ? '' : displayName;

            try {
                const response = await authFetch('/api/cluster/alias', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ profileId: profileId, clusterId: clusterId, alias: alias })
                });
                const result = await readJSON(response);
                if (result.success) {
                    if (alias) {
                        showToast('Renamed: ' + displayName, 'success');
                    } else {
                        showToast('Reset to source name', 'success');
                    }
                    hideEditAliasModal();
                    // Update the row in place; reloading would refetch every cluster.
                    const cluster = clusters[index];
                    if (cluster && cluster.id === clusterId && cluster.profileId === profileId) {
                        cluster.alias = alias;
                        renderClusterTable();
                    } else {
                        fetchClusters();
                    }
                } else {
                    showToast('Failed to save: ' + errorOf(result, response), 'error');
                }
            } catch (error) {
                showToast('Error: ' + error.message, 'error');
            }
        }

        // Load status and profiles on page load
        document.addEventListener('DOMContentLoaded', async () => {
            document.getElementById('profileList').addEventListener('click', onProfileListClick);
            document.getElementById('contextList').addEventListener('click', onContextListClick);
            await loadStatus();
            await loadProfiles();
        });
    </script>
</body>
</html>`
