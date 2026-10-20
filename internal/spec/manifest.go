package spec

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadManifest reads and validates a pkgdepot.yaml. Validation happens once at
// this boundary; every downstream package may rely on non-empty Name, resolved
// image refs, and at least one package source.
func LoadManifest(file string) (*Manifest, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", file, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", file, err)
	}
	m.applyDefaults()
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("invalid manifest %s: %w", file, err)
	}
	return &m, nil
}

func (m *Manifest) applyDefaults() {
	if m.APIVersion == "" {
		m.APIVersion = ManifestAPIVersion
	}
	if m.Images.Verdaccio == "" {
		m.Images.Verdaccio = DefaultVerdaccioImage
	}
	if m.Images.Pypiserver == "" {
		m.Images.Pypiserver = DefaultPypiserverImage
	}
	if m.Python.PythonVersion == "" {
		m.Python.PythonVersion = "3.12"
	}
	if m.Python.Implementation == "" {
		m.Python.Implementation = "cp"
	}
}

// Validate enforces the manifest contract. It rejects only states a user can
// actually author; it does not guard against impossible in-memory values.
func (m *Manifest) Validate() error {
	if m.APIVersion != ManifestAPIVersion {
		return fmt.Errorf("apiVersion must be %q, got %q", ManifestAPIVersion, m.APIVersion)
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.ContainsAny(m.Name, "/\\ \t\n") {
		return fmt.Errorf("name %q must not contain whitespace or path separators", m.Name)
	}
	if len(m.NPM.Lockfiles) == 0 && len(m.NPM.Packages) == 0 &&
		len(m.Python.Requirements) == 0 && len(m.Python.Packages) == 0 {
		return fmt.Errorf("declare at least one npm or python package source")
	}
	if len(m.NPM.Lockfiles) > 0 && len(m.NPM.Packages) > 0 {
		return fmt.Errorf("npm.lockfiles and npm.packages are mutually exclusive")
	}
	if len(m.Python.Requirements) > 0 && len(m.Python.Packages) > 0 {
		return fmt.Errorf("python.requirements and python.packages are mutually exclusive")
	}
	for _, spec := range m.NPM.Packages {
		if _, _, err := splitNPMSpec(spec); err != nil {
			return err
		}
	}
	for _, spec := range m.Python.Packages {
		if strings.TrimSpace(spec) == "" {
			return fmt.Errorf("python package spec must not be empty")
		}
	}
	return nil
}

// splitNPMSpec splits "name@range" while preserving scoped names such as
// "@scope/pkg@1.2.3". Range may be empty.
func splitNPMSpec(s string) (name, rng string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", fmt.Errorf("npm package spec must not be empty")
	}
	at := strings.LastIndex(s, "@")
	if strings.HasPrefix(s, "@") {
		// Scoped: the separator is the second '@'.
		second := strings.Index(s[1:], "@")
		if second < 0 {
			return s, "", nil
		}
		at = second + 1
	}
	if at <= 0 {
		return s, "", nil
	}
	name, rng = s[:at], s[at+1:]
	if strings.TrimSpace(name) == "" {
		return "", "", fmt.Errorf("invalid npm package spec %q", s)
	}
	return name, rng, nil
}

// SplitNPMSpec is the exported form used by the bundler.
func SplitNPMSpec(s string) (name, rng string, err error) { return splitNPMSpec(s) }

// LoadBundleManifest reads a bundle manifest from an extracted bundle directory.
//
// The bundle manifest is JSON (it is produced by this tool and read back by it),
// so it is decoded with encoding/json. Decoding it with the YAML decoder would
// silently produce zero values because the struct carries only json tags.
func LoadBundleManifest(dir string) (*BundleManifest, error) {
	raw, err := os.ReadFile(path.Join(dir, RemoteManifestFile))
	if err != nil {
		return nil, fmt.Errorf("read bundle manifest: %w", err)
	}
	var bm BundleManifest
	if err := json.Unmarshal(raw, &bm); err != nil {
		return nil, fmt.Errorf("parse bundle manifest: %w", err)
	}
	if bm.APIVersion != BundleAPIVersion {
		return nil, fmt.Errorf("bundle manifest apiVersion must be %q, got %q", BundleAPIVersion, bm.APIVersion)
	}
	return &bm, nil
}

// LoadHostsFile reads hosts.yaml. A missing file is an empty inventory, not an
// error: first run must work without any setup step.
func LoadHostsFile(file string) (*HostsFile, error) {
	raw, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return &HostsFile{APIVersion: HostsAPIVersion, Hosts: map[string]*Host{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read hosts file %s: %w", file, err)
	}
	var hf HostsFile
	if err := yaml.Unmarshal(raw, &hf); err != nil {
		return nil, fmt.Errorf("parse hosts file %s: %w", file, err)
	}
	if hf.APIVersion == "" {
		hf.APIVersion = HostsAPIVersion
	}
	if hf.Hosts == nil {
		hf.Hosts = map[string]*Host{}
	}
	return &hf, nil
}

// Save writes hosts.yaml with owner-only permissions because it holds the
// registry publish credential.
func (hf *HostsFile) Save(file string) error {
	if hf.APIVersion == "" {
		hf.APIVersion = HostsAPIVersion
	}
	if err := os.MkdirAll(path.Dir(file), 0o700); err != nil {
		return fmt.Errorf("create hosts dir: %w", err)
	}
	raw, err := yaml.Marshal(hf)
	if err != nil {
		return fmt.Errorf("encode hosts file: %w", err)
	}
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		return fmt.Errorf("write hosts file %s: %w", file, err)
	}
	return nil
}

// Get returns a host by name or a helpful error listing known hosts.
func (hf *HostsFile) Get(name string) (*Host, error) {
	h, ok := hf.Hosts[name]
	if !ok {
		return nil, fmt.Errorf("unknown host %q (known: %s)", name, strings.Join(hf.Names(), ", "))
	}
	return h, nil
}

// Names lists configured host names in stable order.
func (hf *HostsFile) Names() []string {
	names := make([]string, 0, len(hf.Hosts))
	for n := range hf.Hosts {
		names = append(names, n)
	}
	sortStrings(names)
	return names
}

// ApplyDefaults fills the optional host fields with the documented defaults.
// It runs at the boundary where a host is added or loaded, so every consumer
// sees the same concrete values.
func (h *Host) ApplyDefaults() {
	if h.Port == 0 {
		h.Port = 22
	}
	if h.BaseDir == "" {
		h.BaseDir = DefaultBaseDir
	}
	if h.NPMPort == 0 {
		h.NPMPort = DefaultNPMPort
	}
	if h.PythonPort == 0 {
		h.PythonPort = DefaultPythonPort
	}
}

// Endpoint returns the base URL a client inside the target network uses.
func (h *Host) NPMURL() string {
	return "http://" + net.JoinHostPort(h.Address, fmt.Sprint(h.NPMPort)) + "/"
}

// PythonURL returns the pip index URL on the target host.
func (h *Host) PythonURL() string {
	return "http://" + net.JoinHostPort(h.Address, fmt.Sprint(h.PythonPort)) + "/simple/"
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
