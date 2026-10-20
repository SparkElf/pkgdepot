// Package spec defines the authoritative data contracts of pkgdepot: the input
// manifest, the bundle manifest, the host inventory file, and the fixed remote
// layout. Every other package depends on these types and must not redefine them.
package spec

import (
	"time"
)

// APIVersions accepted by this tool.
const (
	ManifestAPIVersion = "pkgdepot/v1"
	BundleAPIVersion   = "pkgdepot.bundle/v1"
	HostsAPIVersion    = "pkgdepot.hosts/v1"
)

// Default container images carried by every bundle. The target host is fully
// offline, so these are the only registry implementations it can ever run.
const (
	DefaultVerdaccioImage  = "verdaccio/verdaccio:6"
	DefaultPypiserverImage = "pypiserver/pypiserver:latest"
)

// Default registry ports inside the target host. They are published on the host
// and are what client .npmrc / pip.conf point at.
const (
	DefaultNPMPort    = 4873
	DefaultPythonPort = 8080
)

// DefaultBaseDir is where a bundle is unpacked and served from on the target.
const DefaultBaseDir = "/opt/pkgdepot"

// Remote layout, relative to Host.BaseDir. install.sh and the CLI verify step
// both read these exact paths, so they are part of the wire contract.
const (
	RemoteManifestFile = "manifest.json"
	RemoteNPMDir       = "npm"
	RemoteNPMConfDir   = "npm/conf"
	RemoteNPMStorage   = "npm/storage"
	RemoteNPMConfig    = "npm/conf/config.yaml"
	RemoteComposeFile  = "compose.yaml"
	RemotePythonDir    = "python"
	RemotePythonPkgs   = "python/packages"
	RemoteImagesDir    = "images"
	RemoteInstallFile  = "install.sh"
	RemoteInventory    = "inventory.json"
)

// Manifest is the user-authored input (pkgdepot.yaml).
type Manifest struct {
	APIVersion string      `yaml:"apiVersion" json:"apiVersion"`
	Kind       string      `yaml:"kind,omitempty" json:"kind,omitempty"`
	Name       string      `yaml:"name" json:"name"`
	NPM        NPMInput    `yaml:"npm,omitempty" json:"npm,omitempty"`
	Python     PythonInput `yaml:"python,omitempty" json:"python,omitempty"`
	Images     ImagesInput `yaml:"images,omitempty" json:"images,omitempty"`
}

// NPMInput declares which npm packages belong in the private registry.
//
// Exactly one of the two forms is used at a time:
//   - Lockfiles: resolve the complete closure recorded by a package-lock.json
//     (or a package.json, resolved with `npm install --package-lock-only`).
//   - Packages: a flat list of "name@range" specs; each is resolved to a
//     version and its transitive closure.
type NPMInput struct {
	Lockfiles  []string `yaml:"lockfiles,omitempty" json:"lockfiles,omitempty"`
	Packages   []string `yaml:"packages,omitempty" json:"packages,omitempty"`
	IncludeDev bool     `yaml:"includeDev,omitempty" json:"includeDev,omitempty"`
}

// PythonInput declares which wheels belong in the private index.
//
// Requirements files are preferred because they express the pinned closure the
// user already validated. Platforms/PythonVersion map directly onto
// `pip download --platform/--python-version`, so one bundle can serve several
// target runtimes.
type PythonInput struct {
	Requirements   []string `yaml:"requirements,omitempty" json:"requirements,omitempty"`
	Packages       []string `yaml:"packages,omitempty" json:"packages,omitempty"`
	Platforms      []string `yaml:"platforms,omitempty" json:"platforms,omitempty"`
	PythonVersion  string   `yaml:"pythonVersion,omitempty" json:"pythonVersion,omitempty"`
	Implementation string   `yaml:"implementation,omitempty" json:"implementation,omitempty"`
}

// ImagesInput overrides the container images carried by a bundle.
type ImagesInput struct {
	Verdaccio  string `yaml:"verdaccio,omitempty" json:"verdaccio,omitempty"`
	Pypiserver string `yaml:"pypiserver,omitempty" json:"pypiserver,omitempty"`
}

// BundleManifest is written as manifest.json at the root of every bundle and is
// the single source of truth for what the bundle contains and how to verify it.
type BundleManifest struct {
	APIVersion  string       `json:"apiVersion"`
	Name        string       `json:"name"`
	CreatedAt   time.Time    `json:"createdAt"`
	ToolVersion string       `json:"toolVersion"`
	Images      []ImageEntry `json:"images"`
	NPM         []NPMEntry   `json:"npm"`
	Python      []PyEntry    `json:"python"`
}

// ImageEntry records one `docker save` archive carried by the bundle.
type ImageEntry struct {
	Ref    string `json:"ref"`
	File   string `json:"file"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// NPMEntry is one npm tarball seeded into the private registry.
//
// Integrity carries the Subresource Integrity string from the upstream
// packument so the target registry can serve it verbatim and clients can verify
// the bytes they download. MetadataJSON is the upstream version object with its
// dependencies closure preserved: dropping it silently breaks transitive
// resolution.
type NPMEntry struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	File         string `json:"file"`
	SHA256       string `json:"sha256"`
	Integrity    string `json:"integrity"`
	MetadataJSON string `json:"metadataJson"`
}

// PyEntry is one Python distribution (wheel or sdist) served by pypiserver.
type PyEntry struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
}

// HostsFile is the persisted host inventory (~/.config/pkgdepot/hosts.yaml).
type HostsFile struct {
	APIVersion string           `yaml:"apiVersion" json:"apiVersion"`
	Hosts      map[string]*Host `yaml:"hosts" json:"hosts"`
}

// Host is one deployable target. SSH secrets are never stored here: IdentityFile
// points at a key on the operator machine, matching common sshd setups inside
// isolated networks.
type Host struct {
	Address      string `yaml:"address" json:"address"`
	Port         int    `yaml:"port,omitempty" json:"port,omitempty"`
	User         string `yaml:"user" json:"user"`
	IdentityFile string `yaml:"identityFile,omitempty" json:"identityFile,omitempty"`
	BaseDir      string `yaml:"baseDir,omitempty" json:"baseDir,omitempty"`
	NPMPort      int    `yaml:"npmPort,omitempty" json:"npmPort,omitempty"`
	PythonPort   int    `yaml:"pythonPort,omitempty" json:"pythonPort,omitempty"`
	LastApply    *Apply `yaml:"lastApply,omitempty" json:"lastApply,omitempty"`
}

// Apply records the last successful deployment, so `status` and `skill export`
// work without connecting to the host.
type Apply struct {
	AppliedAt    time.Time `yaml:"appliedAt" json:"appliedAt"`
	BundleName   string    `yaml:"bundleName" json:"bundleName"`
	BundleDigest string    `yaml:"bundleDigest" json:"bundleDigest"`
	Inventory    Inventory `yaml:"inventory" json:"inventory"`
}

// Inventory is the verified, actually-served package set of a host.
type Inventory struct {
	NPM    []PackageRef `yaml:"npm" json:"npm"`
	Python []PackageRef `yaml:"python" json:"python"`
}

// PackageRef is one served package version.
type PackageRef struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
