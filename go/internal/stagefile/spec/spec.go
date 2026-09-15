package spec

// File is a parsed, not-yet-validated Stagefile source.
type File struct {
	Version int     `yaml:"version"`
	Stages  []Stage `yaml:"stages"`
}

// Stage is one named build stage. Every stage has exactly one base image
// (From) plus whichever optional operations it performs.
type Stage struct {
	Name string `yaml:"name"`
	From string `yaml:"from"`
	// Pin defaults to true (digest-pinned via the lockfile). `pin: false`
	// is a visible deviation for images that exist only in the local
	// daemon store (docker load'd, never pushed) and therefore have no
	// registry digest to pin against.
	Pin *bool `yaml:"pin,omitempty"`
	// Platform may be "" (default) or "build": pin this stage to
	// $BUILDPLATFORM so arch-independent output (bundled frontends, static
	// assets) compiles natively instead of under emulation.
	Platform string `yaml:"platform,omitempty"`
	// Workdir sets the working directory for every instruction in the
	// stage and for the container at runtime (final stage). Must be an
	// absolute path.
	Workdir string `yaml:"workdir,omitempty"`
	// Args declares build arguments (ARG) with optional default values,
	// overridable at build time via --build-arg. Emitted sorted by key.
	Args map[string]string `yaml:"args,omitempty"`
	// Env declares environment variables (ENV), visible to install/build
	// steps in this stage and baked into the final image. Emitted sorted
	// by key.
	Env     map[string]string `yaml:"env,omitempty"`
	Install *Install          `yaml:"install,omitempty"`
	// CUDA declares that this stage runs on the GPU. It carries no options
	// on purpose: everything a CUDA build needs — which CUDA version, which
	// wheel index serves it, which runtime packages accompany it, where those
	// are collected so they aren't shadowed at run time, and that the stage
	// runs as root — follows from the GPU architecture of the device being
	// built for, and is resolved by the compiler (internal/stagefile/gpu).
	//
	// What this replaces is a vendor index URL, thirteen nvidia-* package
	// names, a collect directory, an LD_LIBRARY_PATH and a user, repeated in
	// every GPU app and correct only for the one board whose quirks the
	// author happened to know.
	CUDA bool `yaml:"cuda,omitempty"`
	// Download fetches files from the network into this stage. Every entry
	// is content-pinned; see Download.
	Download    []Download   `yaml:"download,omitempty"`
	Build       *Build       `yaml:"build,omitempty"`
	Copy        []CopyEntry  `yaml:"copy,omitempty"`
	Healthcheck *Healthcheck `yaml:"healthcheck,omitempty"`
	Entrypoint  *Entrypoint  `yaml:"entrypoint,omitempty"`
	// Cmd is the container's default-argument list (CMD), overridable by
	// `docker run <image> <args>` while Entrypoint stays fixed.
	Cmd  []string `yaml:"cmd,omitempty"`
	User string   `yaml:"user,omitempty"`
}

// Download fetches one file from URL into the image at build time. BuildKit
// performs the fetch itself (ADD --checksum), so no shell runs inside the
// container and the bytes are verified before any layer can read them — the
// same mechanism, and the same guarantee, as a pinned apt signing key.
//
// SHA256 is optional in the source and mandatory in the compiled output:
// omitted, it is resolved once against the live URL and recorded in
// build.stagefile.lock.yaml, exactly as an unpinned image ref is, and never
// re-resolved after that without an explicit re-lock.
type Download struct {
	URL string `yaml:"url"`
	// SHA256 pins the content, with or without a "sha256:" prefix.
	SHA256 string `yaml:"sha256,omitempty"`
	// Dest is where the file lands (or, with Extract, the directory the
	// archive is unpacked into). A relative path resolves against the
	// stage's workdir, as copy.dest does.
	Dest string `yaml:"dest"`
	// Extract unpacks the download at Dest instead of placing the file
	// there: "tar.gz" or "zip". A remote ADD never auto-extracts the way a
	// local tarball does, so unpacking is necessarily its own step — which
	// is also why it is emitted after install: and can therefore use a tool
	// (unzip) that install.apt declared.
	Extract string `yaml:"extract,omitempty"`
	// Mode and Owner set the placed file's permissions and ownership
	// (ADD --chmod / --chown). Neither is allowed with Extract, where they
	// would describe one file that no longer exists by the end of the stage.
	Mode  string `yaml:"mode,omitempty"`
	Owner string `yaml:"owner,omitempty"`
}

// ExtractFormats are the archive kinds Extract accepts. Both are unpacked by
// a tool the compiler invokes with typed arguments; adding a format means
// adding its command, not accepting one.
var ExtractFormats = []string{"tar.gz", "zip"}

// LDLibraryPath is the environment variable a CUDA stage prepends its
// collected library directory to. Registering the directory in
// /etc/ld.so.conf.d is not enough on its own: paths injected into the
// container at run time (CDI, on a Jetson) are searched ahead of ld.so.conf
// entries, and only LD_LIBRARY_PATH beats them.
//
// Exported because codegen sets it and validate refuses to let a Stagefile
// overwrite it — the two must agree on the name.
const LDLibraryPath = "LD_LIBRARY_PATH"

// Install is the set of declarative, per-ecosystem dependency installs for
// one stage. Any subset of the fields may be set; each compiles to a fixed
// instruction sequence with the compiler's own correct flags baked in — there
// is no field that accepts a raw install command.
type Install struct {
	Apt   *AptInstall    `yaml:"apt,omitempty"`
	Apk   *ApkInstall    `yaml:"apk,omitempty"`
	CMake []CMakeInstall `yaml:"cmake,omitempty"`
	// Pip is a list because one stage often needs several pip invocations
	// that cannot be merged: a package from a vendor index and its runtime
	// from PyPI must stay separate, since making the vendor index primary
	// and PyPI extra lets pip resolve either package from either source.
	// Groups are emitted in declaration order.
	Pip []PipInstall `yaml:"pip,omitempty"`
	Npm *NpmInstall  `yaml:"npm,omitempty"`
	Uv  *UvInstall   `yaml:"uv,omitempty"`
}

// AptInstall installs Debian/Ubuntu packages, optionally from additional
// declared repositories.
type AptInstall struct {
	Packages []string `yaml:"packages"`
	// Recommends opts back in to apt's Recommends-pulled extras (the
	// compiler passes --no-install-recommends by default).
	Recommends bool `yaml:"recommends,omitempty"`
	// Repositories declares additional apt sources set up before the
	// install: the signing key is fetched by BuildKit itself (ADD with a
	// mandatory sha256 checksum — the same pin-everything stance as the
	// image lockfile) and a sources.list.d entry is written. HTTPS
	// repository URLs additionally require ca-certificates, which the
	// compiler bootstraps from the base image's stock sources first.
	Repositories []AptRepository `yaml:"repositories,omitempty"`
}

// AptRepository is one declared apt source.
type AptRepository struct {
	// Name is a filename-safe identifier used for the keyring
	// (/etc/apt/keyrings/<name>.asc) and list
	// (/etc/apt/sources.list.d/<name>.list) filenames.
	Name       string           `yaml:"name"`
	URL        string           `yaml:"url"`
	Suites     []string         `yaml:"suites"`
	Components []string         `yaml:"components"`
	Key        AptRepositoryKey `yaml:"key"`
}

// AptRepositoryKey pins the repository signing key, fetched from URL and
// verified against SHA256 before it can sign anything. Format tells apt
// how to parse it: "binary" (default — a dearmored OpenPGP keyring, saved
// as .gpg; what ros.key and most vendor instructions ship) or "armored"
// (ASCII "BEGIN PGP PUBLIC KEY BLOCK", saved as .asc).
type AptRepositoryKey struct {
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"`
	Format string `yaml:"format,omitempty"`
}

// ApkInstall installs Alpine packages. Unlike apt there is no Recommends
// concept; Cache opts in to keeping /var/cache/apk in the layer (the
// compiler passes --no-cache by default).
type ApkInstall struct {
	Packages []string `yaml:"packages"`
	Cache    bool     `yaml:"cache,omitempty"`
	// Repositories appends extra repository URLs to /etc/apk/repositories
	// before the install.
	Repositories []string `yaml:"repositories,omitempty"`
}

// CMakeInstall builds and installs one CMake project from a commit-pinned Git
// repository. The base image must provide git, cmake, a compiler, and any
// project-specific development packages (normally through apt or apk above).
// Stagefile emits these installs after OS packages and before language package
// managers, so native libraries are available while pip/npm/etc. build their
// own dependencies. There is deliberately no raw command field.
type CMakeInstall struct {
	Repository string `yaml:"repository"`
	// Commit must be the full 40-hex Git object ID. Tags and branches are not
	// accepted because they can move without changing the Stagefile.
	Commit string `yaml:"commit"`
	// Prefix defaults to /usr/local.
	Prefix string `yaml:"prefix,omitempty"`
	// BuildType defaults to Release. Supported values are CMake's standard
	// single-config build types.
	BuildType string `yaml:"buildType,omitempty"`
	// Defines becomes sorted -DKEY=value arguments.
	Defines map[string]string `yaml:"defines,omitempty"`
	// Jobs sets `cmake --build --parallel N`; zero leaves parallelism to the
	// build tool's default.
	Jobs int `yaml:"jobs,omitempty"`
}

// PipInstall installs Python dependencies from a requirements file, an
// explicit package list, or both, optionally against a non-PyPI index.
type PipInstall struct {
	Requirements string   `yaml:"requirements,omitempty"`
	Packages     []string `yaml:"packages,omitempty"`
	// BuildPackages are OS packages needed only while pip builds wheels (for
	// example, a compiler or development headers). The compiler installs them
	// in pip's independent dependency stage, so they never enter the runtime
	// image. Package names follow the stage's declared apt or apk manager; apt
	// is the fallback when the stage declares neither.
	BuildPackages []string `yaml:"buildPackages,omitempty"`
	// Index replaces the default package index (--index-url), e.g. a
	// Jetson wheel index. ExtraIndex appends additional indexes
	// (--extra-index-url), searched alongside the primary index.
	Index      string   `yaml:"index,omitempty"`
	ExtraIndex []string `yaml:"extraIndex,omitempty"`
	// CUDA resolves this group against the GPU wheel index the target
	// architecture implies, instead of PyPI. It is how a project that
	// deploys to more than one board names its GPU wheels without naming a
	// board: `packages: ["torch==2.8.0"]` with `cuda: true` is a Jetson-6
	// wheel on an Orin and whatever the profile says elsewhere, while the
	// Stagefile stays the same.
	//
	// Mutually exclusive with Index/ExtraIndex, and only meaningful in a
	// stage that declares cuda:.
	CUDA bool `yaml:"cuda,omitempty"`
}

// NpmInstall always compiles to the ecosystem's frozen-lockfile install
// form. Manager selects npm (default), yarn, or pnpm. Production skips
// devDependencies.
type NpmInstall struct {
	Manager    string `yaml:"manager,omitempty"`
	Production bool   `yaml:"production,omitempty"`
}

// UvInstall syncs Python dependencies from pyproject.toml + uv.lock via
// uv (which must be present in the base image). Dev dependencies are
// skipped unless Dev is set; Extras selects optional dependency groups.
type UvInstall struct {
	Extras []string `yaml:"extras,omitempty"`
	Dev    bool     `yaml:"dev,omitempty"`
}

// NpmManifest is the manifest file npm, yarn, and pnpm all read alongside
// their lockfile. Both ir.Lower (which records it on the node, so codegen
// and cachekey share one value) and dockerignore (which allowlists it) use
// this constant, so the compiled build and the build context can't drift on
// which file is present. It lives here, next to NpmLockfile, so the
// manifest and lockfile names are decided in one place.
const NpmManifest = "package.json"

// NpmLockfile returns the lockfile filename a manager value expects
// alongside NpmManifest (empty string defaults to "npm"). Both codegen
// (to COPY and RUN against the right file) and dockerignore (to allowlist
// it) call this so the two packages can't drift out of sync on which file
// each manager uses.
func NpmLockfile(manager string) string {
	switch manager {
	case "yarn":
		return "yarn.lock"
	case "pnpm":
		return "pnpm-lock.yaml"
	default:
		return "package-lock.json"
	}
}

// Build is a per-language compile step. Profile defaults to "release";
// "debug" is an explicit, visible deviation. For npm/yarn/pnpm the "build"
// is running a package.json script (Script, default "build") — Profile
// does not apply there.
type Build struct {
	Lang    string `yaml:"lang"`
	Profile string `yaml:"profile,omitempty"`
	// Product scopes the build to one artifact: `swift build --product X`,
	// `cargo build --bin X`, and for go a package path (e.g. ./cmd/serve)
	// whose binary is installed to /usr/local/bin/. Without it, go's
	// `go build ./...` compiles everything and keeps nothing — declare a
	// product for any stage whose output a later step needs.
	Product string `yaml:"product,omitempty"`
	// Script names the package.json script to run for npm/yarn/pnpm
	// builds; defaults to "build".
	Script string `yaml:"script,omitempty"`
}

// CopyEntry promotes files from a prior stage (or the reserved name
// "local", meaning the source tree next to the Stagefile) into this stage.
// Paths must be non-empty and none may be the literal path "/". Dest is
// required when Paths has more than one entry; otherwise it defaults to
// the single path itself.
type CopyEntry struct {
	From  string   `yaml:"from"`
	Paths []string `yaml:"paths"`
	Dest  string   `yaml:"dest,omitempty"`
	// Owner sets the copied files' ownership (COPY --chown), e.g. "1000:1000".
	Owner string `yaml:"owner,omitempty"`
	// Mode sets the copied files' permissions (COPY --chmod), e.g. "0755".
	Mode string `yaml:"mode,omitempty"`
}

// Healthcheck declares the container health probe (exec form only).
// Interval/Timeout/StartPeriod are Go-style durations ("30s", "1m").
type Healthcheck struct {
	Exec        []string `yaml:"exec"`
	Interval    string   `yaml:"interval,omitempty"`
	Timeout     string   `yaml:"timeout,omitempty"`
	StartPeriod string   `yaml:"startPeriod,omitempty"`
	Retries     int      `yaml:"retries,omitempty"`
}

// Entrypoint is always an argv list — there is no shell-string form.
// Source optionally names a shell file (e.g. a ROS setup.bash) sourced in
// bash before exec'ing Exec; the wrapper passes Exec through untouched as
// "$@", so no Exec argument is ever interpreted by the shell.
type Entrypoint struct {
	Exec   []string `yaml:"exec"`
	Source string   `yaml:"source,omitempty"`
}

// UvLocalFiles is the set of local files a uv install copies into the
// image. Both codegen (to COPY them) and dockerignore (to allowlist them)
// consume it so the two packages can't drift on which files the build
// context must include.
var UvLocalFiles = []string{"pyproject.toml", "uv.lock"}
