// Package web provides a web-based GUI for rancher-kubeconfig-proxy
package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sync"

	"github.com/rancher-kubeconfig-proxy/pkg/config"
	"github.com/rancher-kubeconfig-proxy/pkg/kubeconfig"
	"github.com/rancher-kubeconfig-proxy/pkg/rancher"
)

// Server represents the web server
type Server struct {
	addr string
	mux  *http.ServeMux
}

// ClusterInfo holds cluster information for the API
type ClusterInfo struct {
	Name        string `json:"name"`
	ID          string `json:"id"`
	State       string `json:"state"`
	Provider    string `json:"provider"`
	Description string `json:"description"`
}

// GenerateRequest represents a kubeconfig generation request
type GenerateRequest struct {
	RancherURL            string   `json:"rancherUrl"`
	Token                 string   `json:"token"`
	ClusterPrefix         string   `json:"clusterPrefix"`
	InsecureSkipTLSVerify bool     `json:"insecureSkipTlsVerify"`
	SelectedClusters      []string `json:"selectedClusters"`
}

// APIResponse represents a generic API response
type APIResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// NewServer creates a new web server
func NewServer(addr string) *Server {
	s := &Server{
		addr: addr,
		mux:  http.NewServeMux(),
	}
	s.setupRoutes()
	return s
}

// setupRoutes configures the HTTP routes
func (s *Server) setupRoutes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/clusters", s.handleListClusters)
	s.mux.HandleFunc("/api/generate", s.handleGenerate)
}

// Start starts the web server
func (s *Server) Start() error {
	log.Printf("Starting web server on %s", s.addr)
	log.Printf("Open http://%s in your browser", s.addr)
	return http.ListenAndServe(s.addr, s.mux)
}

// handleIndex serves the main HTML page
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	tmpl, err := template.New("index").Parse(indexHTML)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.Execute(w, nil)
}

// handleListClusters handles the cluster listing API endpoint
func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
		return
	}

	var req GenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	cfg := &config.Config{
		RancherURL:            req.RancherURL,
		Token:                 req.Token,
		InsecureSkipTLSVerify: req.InsecureSkipTLSVerify,
	}

	if err := cfg.Validate(); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Configuration error: %v", err),
		})
		return
	}

	client, err := rancher.NewClient(cfg)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to create Rancher client: %v", err),
		})
		return
	}

	clusters, err := client.ListClusters()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to list clusters: %v", err),
		})
		return
	}

	clusterInfos := make([]ClusterInfo, len(clusters))
	for i, c := range clusters {
		clusterInfos[i] = ClusterInfo{
			Name:        c.Name,
			ID:          c.ID,
			State:       c.State,
			Provider:    c.Provider,
			Description: c.Description,
		}
	}

	s.writeJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    clusterInfos,
	})
}

// handleGenerate handles the kubeconfig generation API endpoint
func (s *Server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, APIResponse{
			Success: false,
			Error:   "Method not allowed",
		})
		return
	}

	var req GenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Invalid request: %v", err),
		})
		return
	}

	cfg := &config.Config{
		RancherURL:            req.RancherURL,
		Token:                 req.Token,
		ClusterPrefix:         req.ClusterPrefix,
		InsecureSkipTLSVerify: req.InsecureSkipTLSVerify,
	}

	if err := cfg.Validate(); err != nil {
		s.writeJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Configuration error: %v", err),
		})
		return
	}

	client, err := rancher.NewClient(cfg)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to create Rancher client: %v", err),
		})
		return
	}

	// Get list of clusters to determine which to include
	clusters, err := client.ListClusters()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to list clusters: %v", err),
		})
		return
	}

	// Build selected cluster set
	selectedSet := make(map[string]bool)
	for _, name := range req.SelectedClusters {
		selectedSet[name] = true
	}

	// Fetch kubeconfigs for selected clusters in parallel
	kubeconfigs := make(map[string]string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	errors := make([]string, 0)

	for _, cluster := range clusters {
		// Skip if not selected (when selection is provided)
		if len(selectedSet) > 0 && !selectedSet[cluster.Name] {
			continue
		}

		// Skip inactive clusters
		if cluster.State != "active" {
			continue
		}

		wg.Add(1)
		go func(c rancher.Cluster) {
			defer wg.Done()

			kubeconfig, err := client.GetClusterKubeconfig(&c)
			if err != nil {
				mu.Lock()
				errors = append(errors, fmt.Sprintf("%s: %v", c.Name, err))
				mu.Unlock()
				return
			}

			mu.Lock()
			kubeconfigs[c.Name] = kubeconfig
			mu.Unlock()
		}(cluster)
	}

	wg.Wait()

	if len(kubeconfigs) == 0 {
		errMsg := "No kubeconfigs retrieved"
		if len(errors) > 0 {
			errMsg = fmt.Sprintf("Failed to get kubeconfigs: %v", errors)
		}
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   errMsg,
		})
		return
	}

	// Generate merged kubeconfig
	generator := kubeconfig.NewGenerator(cfg.ClusterPrefix)
	kubeconfigData, err := generator.Generate(kubeconfigs)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("Failed to generate kubeconfig: %v", err),
		})
		return
	}

	// Return as downloadable file
	w.Header().Set("Content-Type", "application/x-yaml")
	w.Header().Set("Content-Disposition", "attachment; filename=kubeconfig.yaml")
	w.Write(kubeconfigData)
}

// writeJSON writes a JSON response
func (s *Server) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// indexHTML is the embedded HTML template for the web interface
const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Rancher Kubeconfig Generator</title>
    <style>
        :root {
            --primary-color: #2563eb;
            --primary-hover: #1d4ed8;
            --success-color: #16a34a;
            --warning-color: #ca8a04;
            --error-color: #dc2626;
            --bg-color: #f8fafc;
            --card-bg: #ffffff;
            --text-color: #1e293b;
            --text-muted: #64748b;
            --border-color: #e2e8f0;
        }

        * {
            box-sizing: border-box;
            margin: 0;
            padding: 0;
        }

        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Oxygen, Ubuntu, sans-serif;
            background-color: var(--bg-color);
            color: var(--text-color);
            line-height: 1.6;
            min-height: 100vh;
        }

        .container {
            max-width: 900px;
            margin: 0 auto;
            padding: 2rem;
        }

        header {
            text-align: center;
            margin-bottom: 2rem;
        }

        header h1 {
            font-size: 1.875rem;
            font-weight: 700;
            margin-bottom: 0.5rem;
        }

        header p {
            color: var(--text-muted);
        }

        .card {
            background: var(--card-bg);
            border-radius: 0.75rem;
            box-shadow: 0 1px 3px rgba(0, 0, 0, 0.1);
            padding: 1.5rem;
            margin-bottom: 1.5rem;
        }

        .card h2 {
            font-size: 1.125rem;
            font-weight: 600;
            margin-bottom: 1rem;
            padding-bottom: 0.75rem;
            border-bottom: 1px solid var(--border-color);
        }

        .form-group {
            margin-bottom: 1rem;
        }

        .form-group label {
            display: block;
            font-weight: 500;
            margin-bottom: 0.375rem;
            font-size: 0.875rem;
        }

        .form-group input[type="text"],
        .form-group input[type="password"],
        .form-group input[type="url"] {
            width: 100%;
            padding: 0.625rem 0.875rem;
            border: 1px solid var(--border-color);
            border-radius: 0.5rem;
            font-size: 0.875rem;
            transition: border-color 0.15s, box-shadow 0.15s;
        }

        .form-group input:focus {
            outline: none;
            border-color: var(--primary-color);
            box-shadow: 0 0 0 3px rgba(37, 99, 235, 0.1);
        }

        .form-group small {
            display: block;
            color: var(--text-muted);
            font-size: 0.75rem;
            margin-top: 0.25rem;
        }

        .checkbox-group {
            display: flex;
            align-items: center;
            gap: 0.5rem;
        }

        .checkbox-group input[type="checkbox"] {
            width: 1rem;
            height: 1rem;
        }

        .btn {
            display: inline-flex;
            align-items: center;
            justify-content: center;
            padding: 0.625rem 1.25rem;
            font-size: 0.875rem;
            font-weight: 500;
            border-radius: 0.5rem;
            cursor: pointer;
            transition: all 0.15s;
            border: none;
        }

        .btn-primary {
            background-color: var(--primary-color);
            color: white;
        }

        .btn-primary:hover {
            background-color: var(--primary-hover);
        }

        .btn-primary:disabled {
            background-color: var(--text-muted);
            cursor: not-allowed;
        }

        .btn-secondary {
            background-color: var(--bg-color);
            color: var(--text-color);
            border: 1px solid var(--border-color);
        }

        .btn-secondary:hover {
            background-color: var(--border-color);
        }

        .btn-group {
            display: flex;
            gap: 0.75rem;
            margin-top: 1rem;
        }

        .alert {
            padding: 0.875rem 1rem;
            border-radius: 0.5rem;
            margin-bottom: 1rem;
            font-size: 0.875rem;
        }

        .alert-error {
            background-color: #fef2f2;
            border: 1px solid #fecaca;
            color: var(--error-color);
        }

        .alert-success {
            background-color: #f0fdf4;
            border: 1px solid #bbf7d0;
            color: var(--success-color);
        }

        .cluster-list {
            border: 1px solid var(--border-color);
            border-radius: 0.5rem;
            max-height: 300px;
            overflow-y: auto;
        }

        .cluster-item {
            display: flex;
            align-items: center;
            padding: 0.75rem 1rem;
            border-bottom: 1px solid var(--border-color);
        }

        .cluster-item:last-child {
            border-bottom: none;
        }

        .cluster-item input[type="checkbox"] {
            margin-right: 0.75rem;
        }

        .cluster-info {
            flex: 1;
        }

        .cluster-name {
            font-weight: 500;
        }

        .cluster-meta {
            font-size: 0.75rem;
            color: var(--text-muted);
        }

        .cluster-state {
            display: inline-block;
            padding: 0.125rem 0.5rem;
            border-radius: 9999px;
            font-size: 0.75rem;
            font-weight: 500;
        }

        .state-active {
            background-color: #dcfce7;
            color: var(--success-color);
        }

        .state-inactive {
            background-color: #fef9c3;
            color: var(--warning-color);
        }

        .state-error {
            background-color: #fef2f2;
            color: var(--error-color);
        }

        .loading {
            display: flex;
            align-items: center;
            justify-content: center;
            padding: 2rem;
            color: var(--text-muted);
        }

        .spinner {
            width: 1.5rem;
            height: 1.5rem;
            border: 2px solid var(--border-color);
            border-top-color: var(--primary-color);
            border-radius: 50%;
            animation: spin 0.8s linear infinite;
            margin-right: 0.5rem;
        }

        @keyframes spin {
            to { transform: rotate(360deg); }
        }

        .hidden {
            display: none !important;
        }

        .select-all {
            padding: 0.5rem 1rem;
            background-color: var(--bg-color);
            border-bottom: 1px solid var(--border-color);
            font-size: 0.875rem;
        }

        footer {
            text-align: center;
            padding: 1rem;
            color: var(--text-muted);
            font-size: 0.875rem;
        }
    </style>
</head>
<body>
    <div class="container">
        <header>
            <h1>Rancher Kubeconfig Generator</h1>
            <p>Generate kubeconfig files from your Rancher managed clusters</p>
        </header>

        <div id="errorAlert" class="alert alert-error hidden"></div>
        <div id="successAlert" class="alert alert-success hidden"></div>

        <div class="card">
            <h2>Rancher Connection</h2>
            <div class="form-group">
                <label for="rancherUrl">Rancher URL</label>
                <input type="url" id="rancherUrl" placeholder="https://rancher.example.com">
                <small>The URL of your Rancher server</small>
            </div>
            <div class="form-group">
                <label for="token">API Token</label>
                <input type="password" id="token" placeholder="token-xxxxx:yyyyyyy">
                <small>Your Rancher API token (format: access_key:secret_key)</small>
            </div>
            <div class="form-group">
                <label for="clusterPrefix">Cluster Name Prefix</label>
                <input type="text" id="clusterPrefix" placeholder="prod-">
                <small>Optional prefix to add to cluster names in the kubeconfig</small>
            </div>
            <div class="form-group">
                <div class="checkbox-group">
                    <input type="checkbox" id="insecureSkipTls">
                    <label for="insecureSkipTls">Skip TLS certificate verification</label>
                </div>
            </div>
            <div class="btn-group">
                <button class="btn btn-primary" id="fetchClustersBtn" onclick="fetchClusters()">
                    Fetch Clusters
                </button>
            </div>
        </div>

        <div class="card hidden" id="clustersCard">
            <h2>Available Clusters</h2>
            <div id="loadingClusters" class="loading hidden">
                <div class="spinner"></div>
                <span>Loading clusters...</span>
            </div>
            <div id="clusterListContainer" class="hidden">
                <div class="select-all">
                    <div class="checkbox-group">
                        <input type="checkbox" id="selectAll" onchange="toggleSelectAll()">
                        <label for="selectAll">Select all active clusters</label>
                    </div>
                </div>
                <div class="cluster-list" id="clusterList"></div>
            </div>
            <div class="btn-group">
                <button class="btn btn-primary" id="generateBtn" onclick="generateKubeconfig()" disabled>
                    Generate Kubeconfig
                </button>
                <button class="btn btn-secondary" onclick="fetchClusters()">
                    Refresh
                </button>
            </div>
        </div>
    </div>

    <footer>
        Rancher Kubeconfig Proxy
    </footer>

    <script>
        let clusters = [];

        function showError(message) {
            const alert = document.getElementById('errorAlert');
            alert.textContent = message;
            alert.classList.remove('hidden');
            document.getElementById('successAlert').classList.add('hidden');
        }

        function showSuccess(message) {
            const alert = document.getElementById('successAlert');
            alert.textContent = message;
            alert.classList.remove('hidden');
            document.getElementById('errorAlert').classList.add('hidden');
        }

        function hideAlerts() {
            document.getElementById('errorAlert').classList.add('hidden');
            document.getElementById('successAlert').classList.add('hidden');
        }

        function getStateClass(state) {
            if (state === 'active') return 'state-active';
            if (state === 'error' || state === 'unavailable') return 'state-error';
            return 'state-inactive';
        }

        async function fetchClusters() {
            hideAlerts();

            const rancherUrl = document.getElementById('rancherUrl').value.trim();
            const token = document.getElementById('token').value.trim();
            const insecureSkipTls = document.getElementById('insecureSkipTls').checked;

            if (!rancherUrl || !token) {
                showError('Please enter both Rancher URL and API token');
                return;
            }

            const clustersCard = document.getElementById('clustersCard');
            const loadingClusters = document.getElementById('loadingClusters');
            const clusterListContainer = document.getElementById('clusterListContainer');
            const fetchBtn = document.getElementById('fetchClustersBtn');

            clustersCard.classList.remove('hidden');
            loadingClusters.classList.remove('hidden');
            clusterListContainer.classList.add('hidden');
            fetchBtn.disabled = true;

            try {
                const response = await fetch('/api/clusters', {
                    method: 'POST',
                    headers: {
                        'Content-Type': 'application/json',
                    },
                    body: JSON.stringify({
                        rancherUrl: rancherUrl,
                        token: token,
                        insecureSkipTlsVerify: insecureSkipTls,
                    }),
                });

                const result = await response.json();

                if (!result.success) {
                    showError(result.error || 'Failed to fetch clusters');
                    clustersCard.classList.add('hidden');
                    return;
                }

                clusters = result.data || [];
                renderClusterList();
                clusterListContainer.classList.remove('hidden');
                showSuccess('Found ' + clusters.length + ' cluster(s)');

            } catch (error) {
                showError('Failed to connect to server: ' + error.message);
                clustersCard.classList.add('hidden');
            } finally {
                loadingClusters.classList.add('hidden');
                fetchBtn.disabled = false;
            }
        }

        function renderClusterList() {
            const clusterList = document.getElementById('clusterList');
            clusterList.innerHTML = '';

            clusters.forEach((cluster, index) => {
                const div = document.createElement('div');
                div.className = 'cluster-item';
                div.innerHTML = ` + "`" + `
                    <input type="checkbox" id="cluster-${index}"
                           ${cluster.state === 'active' ? 'checked' : ''}
                           ${cluster.state !== 'active' ? 'disabled' : ''}
                           onchange="updateGenerateButton()">
                    <div class="cluster-info">
                        <div class="cluster-name">${escapeHtml(cluster.name)}</div>
                        <div class="cluster-meta">
                            ID: ${escapeHtml(cluster.id)} | Provider: ${escapeHtml(cluster.provider || 'N/A')}
                        </div>
                    </div>
                    <span class="cluster-state ${getStateClass(cluster.state)}">${escapeHtml(cluster.state)}</span>
                ` + "`" + `;
                clusterList.appendChild(div);
            });

            updateGenerateButton();
            updateSelectAll();
        }

        function escapeHtml(text) {
            const div = document.createElement('div');
            div.textContent = text || '';
            return div.innerHTML;
        }

        function toggleSelectAll() {
            const selectAll = document.getElementById('selectAll').checked;
            clusters.forEach((cluster, index) => {
                const checkbox = document.getElementById('cluster-' + index);
                if (checkbox && cluster.state === 'active') {
                    checkbox.checked = selectAll;
                }
            });
            updateGenerateButton();
        }

        function updateSelectAll() {
            const activeClusters = clusters.filter(c => c.state === 'active');
            const checkedCount = activeClusters.filter((c, i) => {
                const checkbox = document.getElementById('cluster-' + clusters.indexOf(c));
                return checkbox && checkbox.checked;
            }).length;
            document.getElementById('selectAll').checked = checkedCount === activeClusters.length && activeClusters.length > 0;
        }

        function updateGenerateButton() {
            const selectedCount = getSelectedClusters().length;
            document.getElementById('generateBtn').disabled = selectedCount === 0;
            updateSelectAll();
        }

        function getSelectedClusters() {
            const selected = [];
            clusters.forEach((cluster, index) => {
                const checkbox = document.getElementById('cluster-' + index);
                if (checkbox && checkbox.checked) {
                    selected.push(cluster.name);
                }
            });
            return selected;
        }

        async function generateKubeconfig() {
            hideAlerts();

            const rancherUrl = document.getElementById('rancherUrl').value.trim();
            const token = document.getElementById('token').value.trim();
            const clusterPrefix = document.getElementById('clusterPrefix').value;
            const insecureSkipTls = document.getElementById('insecureSkipTls').checked;
            const selectedClusters = getSelectedClusters();

            if (selectedClusters.length === 0) {
                showError('Please select at least one cluster');
                return;
            }

            const generateBtn = document.getElementById('generateBtn');
            generateBtn.disabled = true;
            generateBtn.textContent = 'Generating...';

            try {
                const response = await fetch('/api/generate', {
                    method: 'POST',
                    headers: {
                        'Content-Type': 'application/json',
                    },
                    body: JSON.stringify({
                        rancherUrl: rancherUrl,
                        token: token,
                        clusterPrefix: clusterPrefix,
                        insecureSkipTlsVerify: insecureSkipTls,
                        selectedClusters: selectedClusters,
                    }),
                });

                if (response.headers.get('Content-Type')?.includes('application/json')) {
                    const result = await response.json();
                    showError(result.error || 'Failed to generate kubeconfig');
                    return;
                }

                // Download the file
                const blob = await response.blob();
                const url = window.URL.createObjectURL(blob);
                const a = document.createElement('a');
                a.href = url;
                a.download = 'kubeconfig.yaml';
                document.body.appendChild(a);
                a.click();
                window.URL.revokeObjectURL(url);
                a.remove();

                showSuccess('Kubeconfig generated and downloaded successfully!');

            } catch (error) {
                showError('Failed to generate kubeconfig: ' + error.message);
            } finally {
                generateBtn.disabled = false;
                generateBtn.textContent = 'Generate Kubeconfig';
                updateGenerateButton();
            }
        }
    </script>
</body>
</html>`
