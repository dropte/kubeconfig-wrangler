package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/kubeconfig-wrangler/pkg/kubeconfig"
	"github.com/kubeconfig-wrangler/pkg/provider"
)

var (
	okePrefix string
	okeOutput string
)

// okeCmd represents the oke command
var okeCmd = &cobra.Command{
	Use:   "oke",
	Short: "Manage Oracle Cloud (OKE) clusters",
	Long: `Manage Oracle Cloud Infrastructure Container Engine for Kubernetes (OKE)
clusters - list clusters and generate kubeconfig files.

Authentication always uses a profile from the OCI config file (~/.oci/config,
or the file named by --config-file or OCI_CLI_CONFIG_FILE). The profile is
--profile, else OCI_CLI_PROFILE, else DEFAULT. Use --auth security_token for
profiles created with "oci session authenticate".

Without --compartment, every active compartment of the tenancy (and the
tenancy root) is searched; compartments you cannot access are skipped.

Generated kubeconfigs authenticate through the OCI CLI ("oci ce cluster
generate-token"), pinned to the same profile, auth mode and config file, so the
oci CLI must be installed wherever the kubeconfig is used.

Examples:
  # List OKE clusters with the DEFAULT profile in its home region
  kubeconfig-wrangler oke list

  # List clusters in one compartment of another region
  kubeconfig-wrangler oke list --region eu-frankfurt-1 --compartment ocid1.compartment.oc1..xxx

  # Generate kubeconfig using a session-token profile
  kubeconfig-wrangler oke generate --profile my-session --auth security_token -o ~/.kube/oke-config`,
}

var okeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List OKE clusters",
	Long: `List the OKE clusters of the tenancy (or of one compartment) in the selected region.

Examples:
  kubeconfig-wrangler oke list
  kubeconfig-wrangler oke list --profile prod --region us-ashburn-1`,
	RunE: runOKEList,
}

var okeGenerateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate kubeconfig from OKE clusters",
	Long: `Generate a merged kubeconfig file containing all active OKE clusters
of the tenancy (or of one compartment) in the selected region.

The kubeconfig uses the OCI CLI for authentication, so you need the OCI CLI
installed and configured to use the generated kubeconfig.

Examples:
  # Generate kubeconfig for all active clusters
  kubeconfig-wrangler oke generate

  # Use the private API endpoint and a cluster name prefix
  kubeconfig-wrangler oke generate --endpoint private --prefix "oke-"

  # Write to a file
  kubeconfig-wrangler oke generate --output ~/.kube/oke-config`,
	RunE: runOKEGenerate,
}

var okeProfilesCmd = &cobra.Command{
	Use:   "profiles",
	Short: "List profiles in the OCI config file",
	Long: `List the profiles defined in the OCI config file: the file named by
--config-file or OCI_CLI_CONFIG_FILE, else ~/.oci/config.`,
	RunE: runOKEProfiles,
}

var okeCompartmentsCmd = &cobra.Command{
	Use:   "compartments",
	Short: "List the compartments searched for clusters",
	Long: `List the tenancy root and every active compartment of the tenancy (or only
--compartment, when given). These are the compartments "oke list" searches.`,
	RunE: runOKECompartments,
}

var okeRegionsCmd = &cobra.Command{
	Use:   "regions",
	Short: "List OCI regions",
	Long:  `List the OCI commercial regions where OKE clusters can be created.`,
	Run:   runOKERegions,
}

func init() {
	pf := okeCmd.PersistentFlags()
	pf.String("profile", "", "OCI config profile name (env: OCI_CLI_PROFILE, default DEFAULT)")
	pf.String("config-file", "", "OCI config file path (env: OCI_CLI_CONFIG_FILE, default ~/.oci/config)")
	pf.StringP("region", "r", "", "OCI region (default: the profile's region)")
	pf.String("compartment", "", "Compartment OCID to search (default: the whole tenancy)")
	pf.String("auth", "", "OCI auth mode: api_key (default) or security_token")
	pf.String("endpoint", "", "Cluster endpoint for kubeconfigs: public (default), private or vcn_hostname")

	okeGenerateCmd.Flags().StringVarP(&okePrefix, "prefix", "p", "", "Prefix to add to cluster names")
	okeGenerateCmd.Flags().StringVarP(&okeOutput, "output", "o", "", "Output file path (default: stdout)")

	okeCmd.AddCommand(okeListCmd)
	okeCmd.AddCommand(okeGenerateCmd)
	okeCmd.AddCommand(okeProfilesCmd)
	okeCmd.AddCommand(okeCompartmentsCmd)
	okeCmd.AddCommand(okeRegionsCmd)
}

// okeConfigFromFlags builds the OKE configuration from the oke flags. The
// profile and config file fall back to OCI_CLI_PROFILE and
// OCI_CLI_CONFIG_FILE when their flags were not given; an explicit flag
// always wins, even an empty one.
func okeConfigFromFlags(c *cobra.Command, getenv func(string) string) (provider.OKEConfig, error) {
	f := c.Flags()
	str := func(name string) string {
		v, _ := f.GetString(name)
		return strings.TrimSpace(v)
	}
	cfg := provider.OKEConfig{
		ProfileID:   "cli",
		ProfileName: "CLI",
		OCIProfile:  str("profile"),
		ConfigFile:  str("config-file"),
		Region:      str("region"),
		Compartment: str("compartment"),
		Auth:        str("auth"),
		Endpoint:    str("endpoint"),
	}
	if !f.Changed("profile") {
		cfg.OCIProfile = strings.TrimSpace(getenv("OCI_CLI_PROFILE"))
	}
	if !f.Changed("config-file") {
		cfg.ConfigFile = strings.TrimSpace(getenv("OCI_CLI_CONFIG_FILE"))
	}

	switch cfg.Auth {
	case "", "api_key", "security_token":
	default:
		return cfg, fmt.Errorf("invalid --auth %q (want api_key or security_token)", cfg.Auth)
	}
	switch cfg.Endpoint {
	case "", "public", "private", "vcn_hostname":
	default:
		return cfg, fmt.Errorf("invalid --endpoint %q (want public, private or vcn_hostname)", cfg.Endpoint)
	}
	return cfg, nil
}

// newOKEProviderFromFlags creates the OKE provider for a subcommand.
func newOKEProviderFromFlags(c *cobra.Command) (*provider.OKEProvider, error) {
	cfg, err := okeConfigFromFlags(c, os.Getenv)
	if err != nil {
		return nil, err
	}
	p, err := provider.NewOKEProvider(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create OKE provider: %w", err)
	}
	return p, nil
}

// okeCompartmentOf extracts the compartment name from an OKE cluster
// description ("compartment: <name>[, kubernetes <version>]").
func okeCompartmentOf(description string) string {
	s := strings.TrimPrefix(description, "compartment: ")
	if i := strings.LastIndex(s, ", kubernetes "); i >= 0 {
		s = s[:i]
	}
	return s
}

func runOKEList(cmd *cobra.Command, args []string) error {
	p, err := newOKEProviderFromFlags(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()

	fmt.Fprintf(os.Stderr, "Fetching OKE clusters from %s...\n", p.Region())
	clusters, err := p.ListClusters()
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}
	if len(clusters) == 0 {
		fmt.Printf("No OKE clusters found in %s\n", p.Region())
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tREGION\tCOMPARTMENT\tID")
	fmt.Fprintln(w, "----\t-----\t------\t-----------\t--")
	for _, c := range clusters {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", c.Name, c.State, c.Region, okeCompartmentOf(c.Description), c.ID)
	}
	return w.Flush()
}

// okeKubeconfigKey returns the name under which a cluster's kubeconfig is
// merged. OKE names are only unique per compartment, so a repeated name gets
// the tail of the cluster OCID appended.
func okeKubeconfigKey(c provider.ClusterInfo, taken map[string]string) string {
	if _, dup := taken[c.Name]; !dup {
		return c.Name
	}
	suffix := c.ID
	if len(suffix) > 6 {
		suffix = suffix[len(suffix)-6:]
	}
	key := c.Name + "-" + suffix
	for n := 2; ; n++ {
		if _, dup := taken[key]; !dup {
			return key
		}
		key = fmt.Sprintf("%s-%s-%d", c.Name, suffix, n)
	}
}

func runOKEGenerate(cmd *cobra.Command, args []string) error {
	p, err := newOKEProviderFromFlags(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()

	fmt.Fprintf(os.Stderr, "Fetching OKE clusters from %s...\n", p.Region())
	clusters, err := p.ListClusters()
	if err != nil {
		return fmt.Errorf("failed to list clusters: %w", err)
	}
	if len(clusters) == 0 {
		return fmt.Errorf("no OKE clusters found in %s", p.Region())
	}
	fmt.Fprintf(os.Stderr, "Found %d cluster(s)\n", len(clusters))

	kubeconfigs := make(map[string]string)
	var fetchErrs []error
	for _, c := range clusters {
		if !strings.EqualFold(c.State, "ACTIVE") {
			fmt.Fprintf(os.Stderr, "Skipping cluster %s (state: %s)\n", c.Name, c.State)
			continue
		}
		data, err := p.GetKubeconfig(c.ID)
		if err != nil {
			fetchErrs = append(fetchErrs, fmt.Errorf("cluster %s: %w", c.Name, err))
			continue
		}
		key := okeKubeconfigKey(c, kubeconfigs)
		if key != c.Name {
			fmt.Fprintf(os.Stderr, "Cluster name %s is used more than once; naming %s (%s) %s\n", c.Name, c.Name, c.ID, key)
		}
		kubeconfigs[key] = data
	}

	failed := reportClusterFailures(errors.Join(fetchErrs...), len(kubeconfigs))
	if len(kubeconfigs) == 0 {
		if failed > 0 {
			return fmt.Errorf("failed to fetch a kubeconfig for any of the %d active cluster(s)", failed)
		}
		return fmt.Errorf("no active clusters found")
	}

	data, err := kubeconfig.NewGenerator(okePrefix).Generate(kubeconfigs)
	if err != nil {
		return fmt.Errorf("failed to generate kubeconfig: %w", err)
	}

	if okeOutput != "" {
		if err := writeOutputFile(okeOutput, data); err != nil {
			return fmt.Errorf("failed to write kubeconfig to %s: %w", okeOutput, err)
		}
		fmt.Fprintf(os.Stderr, "Kubeconfig written to %s\n", okeOutput)
	} else {
		fmt.Print(string(data))
	}

	if failed > 0 {
		return partialError(failed, len(kubeconfigs))
	}
	return nil
}

// okeConfigFileFromFlags returns the OCI config file named by --config-file,
// else OCI_CLI_CONFIG_FILE, with the same precedence as okeConfigFromFlags
// (an explicit empty flag clears the env value). "" means ~/.oci/config.
func okeConfigFileFromFlags(c *cobra.Command, getenv func(string) string) string {
	path, _ := c.Flags().GetString("config-file")
	if !c.Flags().Changed("config-file") {
		path = getenv("OCI_CLI_CONFIG_FILE")
	}
	return strings.TrimSpace(path)
}

// listOCIProfilesIn lists the profiles of the given OCI config file ("" for
// ~/.oci/config). provider.ListOCIProfiles only reads OCI_CLI_CONFIG_FILE, so
// the variable is pointed at path for the call and restored afterwards; the
// CLI runs one command per process, so nothing else observes the change.
func listOCIProfilesIn(path string) ([]string, error) {
	const env = "OCI_CLI_CONFIG_FILE"
	old, had := os.LookupEnv(env)
	defer func() {
		if had {
			_ = os.Setenv(env, old)
		} else {
			_ = os.Unsetenv(env)
		}
	}()
	var err error
	if path == "" {
		err = os.Unsetenv(env)
	} else {
		err = os.Setenv(env, path)
	}
	if err != nil {
		return nil, err
	}
	return provider.ListOCIProfiles()
}

func runOKEProfiles(cmd *cobra.Command, args []string) error {
	path := okeConfigFileFromFlags(cmd, os.Getenv)
	profiles, err := listOCIProfilesIn(path)
	if err != nil {
		return fmt.Errorf("failed to list OCI profiles: %w", err)
	}
	shown := path
	if shown == "" {
		shown = "~/.oci/config"
	}
	out := cmd.OutOrStdout()
	if len(profiles) == 0 {
		fmt.Fprintf(out, "No OCI profiles found in %s\n", shown)
		return nil
	}
	fmt.Fprintf(out, "Available OCI profiles (%s):\n", shown)
	for _, profile := range profiles {
		fmt.Fprintf(out, "  %s\n", profile)
	}
	return nil
}

func runOKECompartments(cmd *cobra.Command, args []string) error {
	p, err := newOKEProviderFromFlags(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()

	compartments, err := p.ListCompartments()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tID")
	fmt.Fprintln(w, "----\t--")
	for _, c := range compartments {
		fmt.Fprintf(w, "%s\t%s\n", c.Name, c.ID)
	}
	return w.Flush()
}

func runOKERegions(cmd *cobra.Command, args []string) {
	fmt.Println("OCI regions:")
	for _, region := range provider.ListOCIRegions() {
		fmt.Printf("  %s\n", region)
	}
}
