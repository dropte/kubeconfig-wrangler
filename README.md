# Rancher Kubeconfig Proxy

A tool that connects to a Rancher instance and generates a merged kubeconfig file containing all downstream Kubernetes clusters managed by that Rancher instance. The generated kubeconfig can be used by any standard Kubernetes tools like `kubectl`, `helm`, `k9s`, and other applications that support kubeconfig files.

## Features

- **Cluster Discovery**: Automatically discovers all downstream Kubernetes clusters managed by your Rancher instance
- **Kubeconfig Generation**: Generates a standard kubeconfig file compatible with any Kubernetes tooling
- **Configurable Prefix**: Add custom prefixes to cluster names to identify which Rancher instance they belong to
- **Multiple Authentication Methods**:
  - API token authentication (access_key:secret_key)
  - Username/password authentication
- **Multiple Interfaces**:
  - Command-line interface (CLI) for scripting and automation
  - Web-based GUI for interactive use
  - Electron desktop application for a native experience
- **Secure**: Supports TLS verification and custom CA certificates
- **Cross-platform**: Works on Linux, macOS, and Windows

## Installation

### Pre-built Binaries

Download the latest release from the [Releases](https://github.com/your-org/rancher-kubeconfig-proxy/releases) page.

### From Source

```bash
# Clone the repository
git clone https://github.com/your-org/rancher-kubeconfig-proxy.git
cd rancher-kubeconfig-proxy

# Build the CLI
make build

# Or install to your GOPATH
make install
```

### Desktop Application

Download the Electron-based desktop application from the releases page:
- **Linux**: AppImage, .deb, or .rpm
- **macOS**: .dmg
- **Windows**: .exe installer

## Usage

### CLI

#### Generate Kubeconfig

```bash
# Using API token
rancher-kubeconfig-proxy generate \
  --url https://rancher.example.com \
  --token token-xxxxx:yyyyyyyyyyy

# Using username/password
rancher-kubeconfig-proxy generate \
  --url https://rancher.example.com \
  --username admin \
  --password mypassword

# With cluster name prefix
rancher-kubeconfig-proxy generate \
  --url https://rancher.example.com \
  --token token-xxxxx:yyyyyyyyyyy \
  --prefix "prod-"

# Output to a specific file
rancher-kubeconfig-proxy generate \
  --url https://rancher.example.com \
  --username admin \
  --password mypassword \
  --output ~/.kube/rancher-config

# Using access key and secret key separately
rancher-kubeconfig-proxy generate \
  --url https://rancher.example.com \
  --access-key token-xxxxx \
  --secret-key yyyyyyyyyyy
```

#### List Clusters

```bash
# Using API token
rancher-kubeconfig-proxy list \
  --url https://rancher.example.com \
  --token token-xxxxx:yyyyyyyyyyy

# Using username/password
rancher-kubeconfig-proxy list \
  --url https://rancher.example.com \
  --username admin \
  --password mypassword
```

#### Start Web GUI

```bash
rancher-kubeconfig-proxy serve --port 8080
```

Then open http://localhost:8080 in your browser.

### Environment Variables

You can use environment variables instead of command-line flags:

| Variable | Description |
|----------|-------------|
| `RANCHER_URL` | Rancher server URL |
| `RANCHER_TOKEN` | API token (access_key:secret_key) |
| `RANCHER_ACCESS_KEY` | API access key |
| `RANCHER_SECRET_KEY` | API secret key |
| `RANCHER_USERNAME` | Rancher username (for password auth) |
| `RANCHER_PASSWORD` | Rancher password (for password auth) |
| `RANCHER_CLUSTER_PREFIX` | Prefix for cluster names |
| `RANCHER_KUBECONFIG_OUTPUT` | Output file path |
| `RANCHER_INSECURE_SKIP_TLS_VERIFY` | Skip TLS verification (true/false) |
| `RANCHER_CA_CERT` | Path to CA certificate file |

Example using environment variables with API token:

```bash
export RANCHER_URL=https://rancher.example.com
export RANCHER_TOKEN=token-xxxxx:yyyyyyyyyyy
export RANCHER_CLUSTER_PREFIX=prod-

rancher-kubeconfig-proxy generate
```

Example using environment variables with username/password:

```bash
export RANCHER_URL=https://rancher.example.com
export RANCHER_USERNAME=admin
export RANCHER_PASSWORD=mypassword
export RANCHER_CLUSTER_PREFIX=prod-

rancher-kubeconfig-proxy generate
```

### Desktop Application

1. Download and install the desktop application for your platform
2. Launch "Rancher Kubeconfig Proxy"
3. Enter your Rancher URL
4. Choose your authentication method:
   - **API Token**: Enter your Rancher API token (format: access_key:secret_key)
   - **Username/Password**: Enter your Rancher username and password
5. Click "Fetch Clusters" to see available clusters
6. Select the clusters you want to include
7. Optionally add a prefix for cluster names
8. Click "Generate Kubeconfig" to download the file

## Getting a Rancher API Token

1. Log in to your Rancher instance
2. Click on your user avatar in the top-right corner
3. Select "Account & API Keys"
4. Click "Create API Key"
5. Give it a description and optionally set an expiration
6. Copy the generated token (format: `access_key:secret_key`)

## Using the Generated Kubeconfig

### With kubectl

```bash
# Set KUBECONFIG environment variable
export KUBECONFIG=~/.kube/rancher-config

# Or merge with existing kubeconfig
export KUBECONFIG=~/.kube/config:~/.kube/rancher-config

# List available contexts
kubectl config get-contexts

# Switch to a cluster
kubectl config use-context prod-my-cluster
```

### With Other Tools

Most Kubernetes tools respect the `KUBECONFIG` environment variable or have a flag to specify the kubeconfig path:

```bash
# Helm
helm --kubeconfig ~/.kube/rancher-config list

# k9s
k9s --kubeconfig ~/.kube/rancher-config
```

## Development

### Prerequisites

- Go 1.21 or later
- Node.js 18 or later (for Electron app)
- Make

### Building

```bash
# Build CLI for current platform
make build

# Build CLI for all platforms
make build-all

# Run tests
make test

# Run linter
make lint

# Build Electron app for development
make electron-dev

# Build Electron app for distribution
make electron-build-linux
make electron-build-mac
make electron-build-win
```

### Project Structure

```
rancher-kubeconfig-proxy/
├── cmd/                    # CLI commands
│   ├── root.go            # Root command
│   ├── generate.go        # Generate command
│   ├── list.go            # List command
│   └── serve.go           # Web server command
├── pkg/
│   ├── config/            # Configuration handling
│   ├── kubeconfig/        # Kubeconfig generation
│   ├── rancher/           # Rancher API client
│   └── web/               # Web server and GUI
├── electron/              # Electron desktop app
│   ├── main.js           # Electron main process
│   ├── preload.js        # Preload script
│   └── package.json      # Electron dependencies
├── .github/workflows/     # CI/CD pipelines
│   ├── ci.yml            # Continuous integration
│   └── release.yml       # Release builds
├── main.go               # Application entry point
├── Makefile              # Build automation
└── README.md             # This file
```

## License

MIT License - see LICENSE file for details.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add some amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request
