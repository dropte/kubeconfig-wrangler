package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/containerengine"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"gopkg.in/ini.v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// OKEConfig holds the configuration for an Oracle Cloud (OKE) provider.
// No secrets are held here: authentication always goes through a profile in
// the OCI config file.
type OKEConfig struct {
	// ProfileID is the unique identifier for this profile
	ProfileID string

	// ProfileName is the display name for this profile
	ProfileName string

	// OCIProfile is the profile in the OCI config file ("" means DEFAULT)
	OCIProfile string

	// ConfigFile is the OCI config file path ("" means OCI_CLI_CONFIG_FILE,
	// else ~/.oci/config). Only an explicit path is pinned into kubeconfigs.
	ConfigFile string

	// Region overrides the region of the OCI profile ("" means the profile's)
	Region string

	// Compartment restricts discovery to one compartment OCID ("" means the
	// whole tenancy)
	Compartment string

	// Auth is "" or "api_key" (API key signing) or "security_token"
	// (session token from "oci session authenticate")
	Auth string

	// Endpoint selects the cluster endpoint written into kubeconfigs:
	// "" or "public", "private" or "vcn_hostname"
	Endpoint string
}

// OKE auth and endpoint values accepted in OKEConfig (they match the
// profile package constants).
const (
	okeAuthAPIKey          = "api_key"
	okeAuthSecurityToken   = "security_token"
	okeEndpointPublic      = "public"
	okeEndpointPrivate     = "private"
	okeEndpointVCNHostname = "vcn_hostname"

	ociDefaultProfile   = "DEFAULT"
	ociConfigFileEnv    = "OCI_CLI_CONFIG_FILE"
	okeTenancyRootName  = "tenancy root"
	okeCallTimeout      = 30 * time.Second
	okeListConcurrency  = 4
	okeKubeconfigTokenV = "2.0.0"
)

// OKEProvider implements ClusterProvider for Oracle Cloud Container Engine
// for Kubernetes.
type OKEProvider struct {
	config   OKEConfig
	profile  string // effective OCI profile name
	region   string // effective region
	tenancy  string // tenancy OCID
	endpoint containerengine.CreateClusterKubeconfigContentDetailsEndpointEnum

	// configPath is the absolute OCI config file the SDK clients read.
	configPath string
	// pinConfigFile is set when configPath was chosen explicitly or via
	// OCI_CLI_CONFIG_FILE, so the generated kubeconfigs must name it too.
	pinConfigFile bool

	ce  containerengine.ContainerEngineClient
	idc identity.IdentityClient
}

// NewOKEProvider creates an OKE provider that authenticates with the given
// profile of the OCI config file.
func NewOKEProvider(cfg OKEConfig) (*OKEProvider, error) {
	profile := cfg.OCIProfile
	if profile == "" {
		profile = ociDefaultProfile
	}
	if _, err := okeEndpointEnum(cfg.Endpoint); err != nil {
		return nil, err
	}

	path := resolveOCIConfigPath(cfg.ConfigFile)
	if err := checkOCIProfile(path, profile); err != nil {
		return nil, err
	}

	var cp common.ConfigurationProvider
	switch cfg.Auth {
	case "", okeAuthAPIKey:
		cp = common.CustomProfileConfigProvider(path, profile)
	case okeAuthSecurityToken:
		cp = common.CustomProfileSessionTokenConfigProvider(path, profile)
	default:
		return nil, fmt.Errorf("unsupported OCI auth %q (want %s or %s)", cfg.Auth, okeAuthAPIKey, okeAuthSecurityToken)
	}
	if cp == nil {
		return nil, fmt.Errorf("failed to load OCI profile %s from %s", profile, path)
	}

	tenancy, err := cp.TenancyOCID()
	if err != nil {
		return nil, fmt.Errorf("failed to read tenancy from OCI profile %s: %w", profile, err)
	}
	region := cfg.Region
	if region == "" {
		if region, err = cp.Region(); err != nil || region == "" {
			return nil, fmt.Errorf("no region configured for OCI profile %s; set one in %s or pass a region", profile, path)
		}
	}

	ce, err := containerengine.NewContainerEngineClientWithConfigurationProvider(cp)
	if err != nil {
		return nil, fmt.Errorf("failed to create OCI container engine client: %w", err)
	}
	idc, err := identity.NewIdentityClientWithConfigurationProvider(cp)
	if err != nil {
		return nil, fmt.Errorf("failed to create OCI identity client: %w", err)
	}
	if cfg.Region != "" {
		ce.SetRegion(cfg.Region)
		idc.SetRegion(cfg.Region)
	}

	p, err := newOKEProviderWithClients(cfg, ce, idc, tenancy, region)
	if err != nil {
		return nil, err
	}
	// Pin exactly the file the SDK clients were configured from.
	p.configPath = path
	return p, nil
}

// newOKEProviderWithClients builds a provider around ready SDK clients (tests
// point them at fake servers). region is the effective region.
func newOKEProviderWithClients(cfg OKEConfig, ce containerengine.ContainerEngineClient, idc identity.IdentityClient, tenancy, region string) (*OKEProvider, error) {
	endpoint, err := okeEndpointEnum(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	profile := cfg.OCIProfile
	if profile == "" {
		profile = ociDefaultProfile
	}
	return &OKEProvider{
		config:        cfg,
		profile:       profile,
		region:        region,
		tenancy:       tenancy,
		endpoint:      endpoint,
		configPath:    resolveOCIConfigPath(cfg.ConfigFile),
		pinConfigFile: cfg.ConfigFile != "" || os.Getenv(ociConfigFileEnv) != "",
		ce:            ce,
		idc:           idc,
	}, nil
}

// okeEndpointEnum maps an OKEConfig.Endpoint value to the SDK enum.
func okeEndpointEnum(endpoint string) (containerengine.CreateClusterKubeconfigContentDetailsEndpointEnum, error) {
	switch endpoint {
	case "", okeEndpointPublic:
		return containerengine.CreateClusterKubeconfigContentDetailsEndpointPublicEndpoint, nil
	case okeEndpointPrivate:
		return containerengine.CreateClusterKubeconfigContentDetailsEndpointPrivateEndpoint, nil
	case okeEndpointVCNHostname:
		return containerengine.CreateClusterKubeconfigContentDetailsEndpointVcnHostname, nil
	}
	return "", fmt.Errorf("unsupported OKE endpoint %q (want %s, %s or %s)",
		endpoint, okeEndpointPublic, okeEndpointPrivate, okeEndpointVCNHostname)
}

// resolveOCIConfigPath returns the OCI config file to use: the explicit
// path, else OCI_CLI_CONFIG_FILE, else ~/.oci/config. A leading "~" is
// expanded and the result is made absolute, so it stays valid when written
// into a kubeconfig that kubectl runs from another directory.
func resolveOCIConfigPath(explicit string) string {
	path := explicit
	if path == "" {
		path = os.Getenv(ociConfigFileEnv)
	}
	home, _ := os.UserHomeDir()
	switch path {
	case "":
		path = filepath.Join(home, ".oci", "config")
	case "~":
		path = home
	default:
		if rest, ok := strings.CutPrefix(path, "~/"); ok {
			path = filepath.Join(home, rest)
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// loadOCIConfigFile parses an OCI config file. OCI profiles are case
// sensitive and values may contain "#" (e.g. key file paths), so inline
// comments are not stripped.
func loadOCIConfigFile(path string) (*ini.File, error) {
	return ini.LoadSources(ini.LoadOptions{IgnoreInlineComment: true}, path)
}

// checkOCIProfile makes sure the profile exists in the config file. The SDK
// silently falls back to DEFAULT for a missing profile, which would make the
// API calls use other credentials than the generated kubeconfigs.
func checkOCIProfile(path, profile string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("OCI config file %s not found: %w", path, err)
	}
	f, err := loadOCIConfigFile(path)
	if err != nil {
		return fmt.Errorf("failed to parse OCI config file %s: %w", path, err)
	}
	if !slices.Contains(ociProfileNames(f), profile) {
		return fmt.Errorf("profile %s not found in OCI config file %s", profile, path)
	}
	return nil
}

// ociProfileNames returns the profiles of a parsed OCI config file. The ini
// package folds [DEFAULT] into its unnamed default section, so DEFAULT is
// reported when that section has any keys.
func ociProfileNames(f *ini.File) []string {
	var names []string
	for _, section := range f.Sections() {
		name := section.Name()
		if name == ini.DefaultSection {
			if len(section.Keys()) > 0 {
				names = append(names, ociDefaultProfile)
			}
			continue
		}
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return slices.Compact(names)
}

// Name returns the display name of this provider instance
func (p *OKEProvider) Name() string {
	return p.config.ProfileName
}

// Type returns the provider type
func (p *OKEProvider) Type() string {
	return "oke"
}

// ProfileID returns the profile ID
func (p *OKEProvider) ProfileID() string {
	return p.config.ProfileID
}

// Region returns the effective region (the override or the profile's).
func (p *OKEProvider) Region() string {
	return p.region
}

// EmbedsCredentials reports whether generated kubeconfigs contain secrets.
// OKE kubeconfigs always authenticate through the oci CLI, so never.
func (p *OKEProvider) EmbedsCredentials() bool {
	return false
}

// OCICompartment is a compartment that ListCompartments returned.
type OCICompartment struct {
	ID   string
	Name string
}

// ListCompartments returns the compartments to search for clusters: the
// configured compartment alone, or the tenancy root followed by every
// active compartment of the tenancy (sorted by name).
func (p *OKEProvider) ListCompartments() ([]OCICompartment, error) {
	if c := p.config.Compartment; c != "" {
		return []OCICompartment{{ID: c, Name: p.compartmentName(c)}}, nil
	}

	var out []OCICompartment
	var page *string
	for {
		ctx, cancel := context.WithTimeout(context.Background(), okeCallTimeout)
		resp, err := p.idc.ListCompartments(ctx, identity.ListCompartmentsRequest{
			CompartmentId:          common.String(p.tenancy),
			CompartmentIdInSubtree: common.Bool(true),
			AccessLevel:            identity.ListCompartmentsAccessLevelAny,
			LifecycleState:         identity.CompartmentLifecycleStateActive,
			Page:                   page,
		})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("failed to list OCI compartments: %w", err)
		}
		for _, c := range resp.Items {
			if c.Id == nil {
				continue
			}
			out = append(out, OCICompartment{ID: *c.Id, Name: deref(c.Name, *c.Id)})
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			break
		}
		page = resp.OpcNextPage
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return append([]OCICompartment{{ID: p.tenancy, Name: okeTenancyRootName}}, out...), nil
}

// compartmentName looks up a single compartment's name, falling back to its
// OCID when the lookup fails.
func (p *OKEProvider) compartmentName(id string) string {
	if id == p.tenancy {
		return okeTenancyRootName
	}
	ctx, cancel := context.WithTimeout(context.Background(), okeCallTimeout)
	defer cancel()
	resp, err := p.idc.GetCompartment(ctx, identity.GetCompartmentRequest{CompartmentId: common.String(id)})
	if err != nil || resp.Name == nil || *resp.Name == "" {
		if err != nil {
			log.Printf("OKE: failed to look up compartment %s: %v", id, err)
		}
		return id
	}
	return *resp.Name
}

// ListClusters lists the clusters of every compartment in scope, sorted by
// name. It implements ClusterProvider; per-compartment failures other than
// 404 are only logged. Use ListClustersWithWarnings to see them.
func (p *OKEProvider) ListClusters() ([]ClusterInfo, error) {
	clusters, _, err := p.ListClustersWithWarnings()
	return clusters, err
}

// ListClustersWithWarnings lists the clusters of every compartment in scope,
// sorted by name. Deleted and deleting clusters are skipped. A compartment
// the caller cannot see (404 / NotAuthorizedOrNotFound) is skipped silently;
// every other per-compartment failure is logged and returned in warnings.
// err is non-nil only when the compartment enumeration fails or no cluster
// listing succeeded.
func (p *OKEProvider) ListClustersWithWarnings() (clusters []ClusterInfo, warnings []error, err error) {
	compartments, err := p.ListCompartments()
	if err != nil {
		return nil, nil, err
	}

	type result struct {
		clusters []ClusterInfo
		err      error
	}
	results := make([]result, len(compartments))
	sem := make(chan struct{}, okeListConcurrency)
	var wg sync.WaitGroup
	for i, c := range compartments {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			clusters, err := p.listCompartmentClusters(c)
			results[i] = result{clusters, err}
		}()
	}
	wg.Wait()

	var errs []error
	var unauthorized []string
	succeeded := 0
	for i, r := range results {
		c := compartments[i]
		if r.err != nil {
			wrapped := fmt.Errorf("compartment %s (%s): %w", c.Name, c.ID, r.err)
			if isOCINotFound(r.err) {
				// Normal for users whose IAM policies cover only some
				// compartments; summarised in one line below.
				unauthorized = append(unauthorized, c.Name)
			} else {
				log.Printf("OKE: failed to list clusters in compartment %s (%s): %v", c.Name, c.ID, r.err)
				warnings = append(warnings, wrapped)
			}
			errs = append(errs, wrapped)
			continue
		}
		succeeded++
		clusters = append(clusters, r.clusters...)
	}
	if len(unauthorized) > 0 {
		log.Printf("OKE: skipped %d of %d compartments (not found or not authorized to list clusters): %s",
			len(unauthorized), len(compartments), strings.Join(unauthorized, ", "))
	}
	if succeeded == 0 && len(errs) > 0 {
		return nil, nil, fmt.Errorf("failed to list OKE clusters in any compartment: %w", errors.Join(errs...))
	}

	sort.SliceStable(clusters, func(i, j int) bool {
		if clusters[i].Name != clusters[j].Name {
			return clusters[i].Name < clusters[j].Name
		}
		return clusters[i].ID < clusters[j].ID
	})
	return clusters, warnings, nil
}

// listCompartmentClusters pages through ListClusters for one compartment.
func (p *OKEProvider) listCompartmentClusters(c OCICompartment) ([]ClusterInfo, error) {
	var out []ClusterInfo
	var page *string
	for {
		ctx, cancel := context.WithTimeout(context.Background(), okeCallTimeout)
		resp, err := p.ce.ListClusters(ctx, containerengine.ListClustersRequest{
			CompartmentId: common.String(c.ID),
			Page:          page,
		})
		cancel()
		if err != nil {
			return nil, err
		}
		for _, s := range resp.Items {
			if s.Id == nil {
				continue
			}
			switch s.LifecycleState {
			case containerengine.ClusterLifecycleStateDeleted, containerengine.ClusterLifecycleStateDeleting:
				continue
			}
			desc := "compartment: " + c.Name
			if s.KubernetesVersion != nil && *s.KubernetesVersion != "" {
				desc += ", kubernetes " + *s.KubernetesVersion
			}
			state := string(s.LifecycleState)
			if state == "" {
				state = "unknown"
			}
			out = append(out, ClusterInfo{
				ID:          *s.Id,
				Name:        deref(s.Name, *s.Id),
				State:       state,
				Provider:    "oke",
				ProfileID:   p.config.ProfileID,
				ProfileName: p.config.ProfileName,
				Region:      p.region,
				Description: desc,
			})
		}
		if resp.OpcNextPage == nil || *resp.OpcNextPage == "" {
			return out, nil
		}
		page = resp.OpcNextPage
	}
}

// isOCINotFound reports whether err is a 404 (NotAuthorizedOrNotFound).
func isOCINotFound(err error) bool {
	se, ok := common.IsServiceError(err)
	return ok && (se.GetHTTPStatusCode() == http.StatusNotFound || se.GetCode() == "NotAuthorizedOrNotFound")
}

// GetKubeconfig fetches the kubeconfig of a cluster (by OCID) and pins the
// oci CLI exec arguments to this provider's profile, auth mode and config
// file so kubectl authenticates the same way regardless of OCI_CLI_* env.
func (p *OKEProvider) GetKubeconfig(clusterID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), okeCallTimeout)
	defer cancel()

	resp, err := p.ce.CreateKubeconfig(ctx, containerengine.CreateKubeconfigRequest{
		ClusterId: common.String(clusterID),
		CreateClusterKubeconfigContentDetails: containerengine.CreateClusterKubeconfigContentDetails{
			TokenVersion: common.String(okeKubeconfigTokenV),
			Endpoint:     p.endpoint,
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to create kubeconfig for cluster %s: %w", clusterID, err)
	}
	if resp.Content == nil {
		return "", fmt.Errorf("empty kubeconfig returned for cluster %s", clusterID)
	}
	defer resp.Content.Close()
	data, err := io.ReadAll(resp.Content)
	if err != nil {
		return "", fmt.Errorf("failed to read kubeconfig for cluster %s: %w", clusterID, err)
	}
	return p.pinKubeconfig(clusterID, data)
}

// pinKubeconfig rewrites the exec args of an OCI-generated kubeconfig.
func (p *OKEProvider) pinKubeconfig(clusterID string, data []byte) (string, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return "", fmt.Errorf("empty kubeconfig returned for cluster %s", clusterID)
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return "", fmt.Errorf("invalid kubeconfig returned for cluster %s: %w", clusterID, err)
	}
	if len(cfg.Clusters) == 0 || len(cfg.Contexts) == 0 {
		return "", fmt.Errorf("kubeconfig returned for cluster %s has no clusters or contexts", clusterID)
	}

	for _, ai := range cfg.AuthInfos {
		if ai == nil || ai.Exec == nil {
			continue
		}
		ai.Exec.Args = p.pinExecArgs(ai.Exec.Args)
		ai.Exec.InteractiveMode = clientcmdapi.NeverExecInteractiveMode
	}

	out, err := clientcmd.Write(*cfg)
	if err != nil {
		return "", fmt.Errorf("failed to serialize kubeconfig for cluster %s: %w", clusterID, err)
	}
	return string(out), nil
}

// pinExecArgs appends --profile, --auth and --config-file to oci CLI args
// unless they are already present.
func (p *OKEProvider) pinExecArgs(args []string) []string {
	has := func(flag string) bool {
		for _, a := range args {
			if a == flag || strings.HasPrefix(a, flag+"=") {
				return true
			}
		}
		return false
	}
	out := slices.Clone(args)
	if !has("--profile") {
		out = append(out, "--profile", p.profile)
	}
	// Always pin the auth mode: the oci CLI otherwise takes it from
	// OCI_CLI_AUTH at kubectl time.
	if !has("--auth") {
		auth := p.config.Auth
		if auth == "" {
			auth = okeAuthAPIKey
		}
		out = append(out, "--auth", auth)
	}
	if p.pinConfigFile && !has("--config-file") {
		out = append(out, "--config-file", p.configPath)
	}
	return out
}

// Close releases idle HTTP connections held by the SDK clients. The SDK
// wraps the real transport in an OciHTTPTransportWrapper, which does not
// implement CloseIdleConnections, so the delegate is reached directly.
func (p *OKEProvider) Close() error {
	for _, d := range []common.HTTPRequestDispatcher{p.ce.HTTPClient, p.idc.HTTPClient} {
		hc, ok := d.(*http.Client)
		if !ok || hc == nil {
			continue
		}
		if w, ok := hc.Transport.(*common.OciHTTPTransportWrapper); ok {
			if ci, ok := w.Delegate().(interface{ CloseIdleConnections() }); ok {
				ci.CloseIdleConnections()
			}
			continue
		}
		hc.CloseIdleConnections()
	}
	return nil
}

func deref(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}

// ListOCIProfiles returns the sorted profile names of the OCI config file
// (OCI_CLI_CONFIG_FILE or ~/.oci/config). It returns nil, nil when the file
// does not exist.
func ListOCIProfiles() ([]string, error) {
	path := resolveOCIConfigPath("")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	f, err := loadOCIConfigFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to parse OCI config file %s: %w", path, err)
	}
	return ociProfileNames(f), nil
}

// ListOCIRegions returns the OCI commercial (oc1 realm) region identifiers,
// sorted.
func ListOCIRegions() []string {
	return []string{
		"af-casablanca-1",
		"af-johannesburg-1",
		"ap-batam-1",
		"ap-chuncheon-1",
		"ap-delhi-1",
		"ap-hyderabad-1",
		"ap-kulai-2",
		"ap-melbourne-1",
		"ap-mumbai-1",
		"ap-osaka-1",
		"ap-seoul-1",
		"ap-singapore-1",
		"ap-singapore-2",
		"ap-sydney-1",
		"ap-tokyo-1",
		"ca-montreal-1",
		"ca-toronto-1",
		"eu-amsterdam-1",
		"eu-frankfurt-1",
		"eu-madrid-1",
		"eu-madrid-3",
		"eu-marseille-1",
		"eu-milan-1",
		"eu-paris-1",
		"eu-stockholm-1",
		"eu-turin-1",
		"eu-zurich-1",
		"il-jerusalem-1",
		"me-abudhabi-1",
		"me-dubai-1",
		"me-jeddah-1",
		"me-riyadh-1",
		"mx-monterrey-1",
		"mx-queretaro-1",
		"sa-bogota-1",
		"sa-santiago-1",
		"sa-saopaulo-1",
		"sa-valparaiso-1",
		"sa-vinhedo-1",
		"uk-cardiff-1",
		"uk-london-1",
		"us-ashburn-1",
		"us-chicago-1",
		"us-phoenix-1",
		"us-saltlake-2",
		"us-sanjose-1",
	}
}
