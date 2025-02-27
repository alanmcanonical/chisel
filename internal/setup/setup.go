package setup

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/crypto/openpgp/packet"
	"gopkg.in/yaml.v3"

	"github.com/canonical/chisel/internal/deb"
	"github.com/canonical/chisel/internal/pgputil"
	"github.com/canonical/chisel/internal/strdist"
)

// Release is a collection of package slices targeting a particular
// distribution version.
type Release struct {
	Path           string
	Packages       map[string]*Package
	Archives       map[string]*Archive
	DefaultArchive string
}

// Archive is the location from which binary packages are obtained.
type Archive struct {
	Name       string
	Version    string
	Suites     []string
	Components []string
	PubKeys    []*packet.PublicKey
}

// Package holds a collection of slices that represent parts of themselves.
type Package struct {
	Name    string
	Path    string
	Archive string
	Slices  map[string]*Slice
}

func (p *Package) MarshalYAML() (interface{}, error) {
	return packageToYAML(p)
}

var _ yaml.Marshaler = (*Package)(nil)

// Slice holds the details about a package slice.
type Slice struct {
	Package   string
	Name      string
	Essential []SliceKey
	Contents  map[string]PathInfo
	Scripts   SliceScripts
}

type SliceScripts struct {
	Mutate string
}

type PathKind string

const (
	DirPath      PathKind = "dir"
	CopyPath     PathKind = "copy"
	GlobPath     PathKind = "glob"
	TextPath     PathKind = "text"
	SymlinkPath  PathKind = "symlink"
	GeneratePath PathKind = "generate"

	// TODO Maybe in the future, for binary support.
	//Base64Path PathKind = "base64"
)

type PathUntil string

const (
	UntilNone   PathUntil = ""
	UntilMutate PathUntil = "mutate"
)

type GenerateKind string

const (
	GenerateNone     GenerateKind = ""
	GenerateManifest GenerateKind = "manifest"
)

type PathInfo struct {
	Kind PathKind
	Info string
	Mode uint

	Mutable  bool
	Until    PathUntil
	Arch     []string
	Generate GenerateKind
}

// SameContent returns whether the path has the same content properties as some
// other path. In other words, the resulting file/dir entry is the same. The
// Mutable flag must also match, as that's a common agreement that the actual
// content is not well defined upfront.
func (pi *PathInfo) SameContent(other *PathInfo) bool {
	return (pi.Kind == other.Kind &&
		pi.Info == other.Info &&
		pi.Mode == other.Mode &&
		pi.Mutable == other.Mutable &&
		pi.Generate == other.Generate)
}

type SliceKey struct {
	Package string
	Slice   string
}

func (s *Slice) String() string   { return s.Package + "_" + s.Name }
func (s SliceKey) String() string { return s.Package + "_" + s.Slice }

// Selection holds the required configuration to create a Build for a selection
// of slices from a Release. It's still an abstract proposal in the sense that
// the real information coming from pacakges is still unknown, so referenced
// paths could potentially be missing, for example.
type Selection struct {
	Release *Release
	Slices  []*Slice
}

func ReadRelease(dir string) (*Release, error) {
	logDir := dir
	if strings.Contains(dir, "/.cache/") {
		logDir = filepath.Base(dir)
	}
	logf("Processing %s release...", logDir)

	release := &Release{
		Path:     dir,
		Packages: make(map[string]*Package),
	}

	release, err := readRelease(dir)
	if err != nil {
		return nil, err
	}

	err = release.validate()
	if err != nil {
		return nil, err
	}
	return release, nil
}

func (r *Release) validate() error {
	keys := []SliceKey(nil)

	// Check for info conflicts and prepare for following checks. A conflict
	// means that two slices attempt to extract different files or directories
	// to the same location.
	// Conflict validation is done without downloading packages which means that
	// if we are extracting content from different packages to the same location
	// we cannot be sure that it will be the same. On the contrary, content
	// extracted from the same package will never conflict because it is
	// guaranteed to be the same.
	// The above also means that generated content (e.g. text files, directories
	// with make:true) will always conflict with extracted content, because we
	// cannot validate that they are the same without downloading the package.
	paths := make(map[string]*Slice)
	globs := make(map[string]*Slice)
	for _, pkg := range r.Packages {
		for _, new := range pkg.Slices {
			keys = append(keys, SliceKey{pkg.Name, new.Name})
			for newPath, newInfo := range new.Contents {
				if old, ok := paths[newPath]; ok {
					oldInfo := old.Contents[newPath]
					if !newInfo.SameContent(&oldInfo) || (newInfo.Kind == CopyPath || newInfo.Kind == GlobPath) && new.Package != old.Package {
						if old.Package > new.Package || old.Package == new.Package && old.Name > new.Name {
							old, new = new, old
						}
						return fmt.Errorf("slices %s and %s conflict on %s", old, new, newPath)
					}
					// Note: Because for conflict resolution we only check that
					// the created file would be the same and we know newInfo and
					// oldInfo produce the same one, we do not have to record
					// newInfo.
				} else {
					paths[newPath] = new
					if newInfo.Kind == GeneratePath || newInfo.Kind == GlobPath {
						globs[newPath] = new
					}
				}
			}
		}
	}

	// Check for glob and generate conflicts.
	for oldPath, old := range globs {
		oldInfo := old.Contents[oldPath]
		for newPath, new := range paths {
			if oldPath == newPath {
				// Identical paths have been filtered earlier. This must be the
				// exact same entry.
				continue
			}
			newInfo := new.Contents[newPath]
			if oldInfo.Kind == GlobPath && (newInfo.Kind == GlobPath || newInfo.Kind == CopyPath) {
				if new.Package == old.Package {
					continue
				}
			}
			if strdist.GlobPath(newPath, oldPath) {
				if (old.Package > new.Package) || (old.Package == new.Package && old.Name > new.Name) ||
					(old.Package == new.Package && old.Name == new.Name && oldPath > newPath) {
					old, new = new, old
					oldPath, newPath = newPath, oldPath
				}
				return fmt.Errorf("slices %s and %s conflict on %s and %s", old, new, oldPath, newPath)
			}
		}
	}

	// Check for cycles.
	_, err := order(r.Packages, keys)
	if err != nil {
		return err
	}

	return nil
}

func order(pkgs map[string]*Package, keys []SliceKey) ([]SliceKey, error) {

	// Preprocess the list to improve error messages.
	for _, key := range keys {
		if pkg, ok := pkgs[key.Package]; !ok {
			return nil, fmt.Errorf("slices of package %q not found", key.Package)
		} else if _, ok := pkg.Slices[key.Slice]; !ok {
			return nil, fmt.Errorf("slice %s not found", key)
		}
	}

	// Collect all relevant package slices.
	successors := map[string][]string{}
	pending := append([]SliceKey(nil), keys...)

	seen := make(map[SliceKey]bool)
	for i := 0; i < len(pending); i++ {
		key := pending[i]
		if seen[key] {
			continue
		}
		seen[key] = true
		pkg := pkgs[key.Package]
		slice := pkg.Slices[key.Slice]
		fqslice := slice.String()
		predecessors := successors[fqslice]
		for _, req := range slice.Essential {
			fqreq := req.String()
			if reqpkg, ok := pkgs[req.Package]; !ok || reqpkg.Slices[req.Slice] == nil {
				return nil, fmt.Errorf("%s requires %s, but slice is missing", fqslice, fqreq)
			}
			predecessors = append(predecessors, fqreq)
		}
		successors[fqslice] = predecessors
		pending = append(pending, slice.Essential...)
	}

	// Sort them up.
	var order []SliceKey
	for _, names := range tarjanSort(successors) {
		if len(names) > 1 {
			return nil, fmt.Errorf("essential loop detected: %s", strings.Join(names, ", "))
		}
		name := names[0]
		dot := strings.IndexByte(name, '_')
		order = append(order, SliceKey{name[:dot], name[dot+1:]})
	}

	return order, nil
}

// fnameExp matches the slice definition file basename.
var fnameExp = regexp.MustCompile(`^([a-z0-9](?:-?[.a-z0-9+]){1,})\.yaml$`)

// snameExp matches only the slice name, without the leading package name.
var snameExp = regexp.MustCompile(`^([a-z](?:-?[a-z0-9]){2,})$`)

// knameExp matches the slice full name in pkg_slice format.
var knameExp = regexp.MustCompile(`^([a-z0-9](?:-?[.a-z0-9+]){1,})_([a-z](?:-?[a-z0-9]){2,})$`)

func ParseSliceKey(sliceKey string) (SliceKey, error) {
	match := knameExp.FindStringSubmatch(sliceKey)
	if match == nil {
		return SliceKey{}, fmt.Errorf("invalid slice reference: %q", sliceKey)
	}
	return SliceKey{match[1], match[2]}, nil
}

func readRelease(baseDir string) (*Release, error) {
	baseDir = filepath.Clean(baseDir)
	filePath := filepath.Join(baseDir, "chisel.yaml")
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("cannot read release definition: %s", err)
	}
	release, err := parseRelease(baseDir, filePath, data)
	if err != nil {
		return nil, err
	}
	err = readSlices(release, baseDir, filepath.Join(baseDir, "slices"))
	if err != nil {
		return nil, err
	}
	return release, err
}

func readSlices(release *Release, baseDir, dirName string) error {
	entries, err := os.ReadDir(dirName)
	if err != nil {
		return fmt.Errorf("cannot read %s%c directory", stripBase(baseDir, dirName), filepath.Separator)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			err := readSlices(release, baseDir, filepath.Join(dirName, entry.Name()))
			if err != nil {
				return err
			}
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		match := fnameExp.FindStringSubmatch(entry.Name())
		if match == nil {
			return fmt.Errorf("invalid slice definition filename: %q", entry.Name())
		}

		pkgName := match[1]
		pkgPath := filepath.Join(dirName, entry.Name())
		if pkg, ok := release.Packages[pkgName]; ok {
			return fmt.Errorf("package %q slices defined more than once: %s and %s\")", pkgName, pkg.Path, pkgPath)
		}
		data, err := os.ReadFile(pkgPath)
		if err != nil {
			// Errors from package os generally include the path.
			return fmt.Errorf("cannot read slice definition file: %v", err)
		}

		pkg, err := parsePackage(baseDir, pkgName, stripBase(baseDir, pkgPath), data)
		if err != nil {
			return err
		}
		if pkg.Archive == "" {
			pkg.Archive = release.DefaultArchive
		}

		release.Packages[pkg.Name] = pkg
	}
	return nil
}

type yamlRelease struct {
	Format   string                 `yaml:"format"`
	Archives map[string]yamlArchive `yaml:"archives"`
	PubKeys  map[string]yamlPubKey  `yaml:"public-keys"`
	// V1PubKeys is used for compatibility with format "chisel-v1".
	V1PubKeys map[string]yamlPubKey `yaml:"v1-public-keys"`
}

type yamlArchive struct {
	Version    string   `yaml:"version"`
	Suites     []string `yaml:"suites"`
	Components []string `yaml:"components"`
	Default    bool     `yaml:"default"`
	PubKeys    []string `yaml:"public-keys"`
	// V1PubKeys is used for compatibility with format "chisel-v1".
	V1PubKeys []string `yaml:"v1-public-keys"`
}

type yamlPackage struct {
	Name      string               `yaml:"package"`
	Archive   string               `yaml:"archive,omitempty"`
	Essential []string             `yaml:"essential,omitempty"`
	Slices    map[string]yamlSlice `yaml:"slices,omitempty"`
}

type yamlPath struct {
	Dir      bool         `yaml:"make,omitempty"`
	Mode     yamlMode     `yaml:"mode,omitempty"`
	Copy     string       `yaml:"copy,omitempty"`
	Text     *string      `yaml:"text,omitempty"`
	Symlink  string       `yaml:"symlink,omitempty"`
	Mutable  bool         `yaml:"mutable,omitempty"`
	Until    PathUntil    `yaml:"until,omitempty"`
	Arch     yamlArch     `yaml:"arch,omitempty"`
	Generate GenerateKind `yaml:"generate,omitempty"`
}

func (yp *yamlPath) MarshalYAML() (interface{}, error) {
	type flowPath *yamlPath
	node := &yaml.Node{}
	err := node.Encode(flowPath(yp))
	if err != nil {
		return nil, err
	}
	node.Style |= yaml.FlowStyle
	return node, nil
}

var _ yaml.Marshaler = (*yamlPath)(nil)

// SameContent returns whether the path has the same content properties as some
// other path. In other words, the resulting file/dir entry is the same. The
// Mutable flag must also match, as that's a common agreement that the actual
// content is not well defined upfront.
func (yp *yamlPath) SameContent(other *yamlPath) bool {
	return (yp.Dir == other.Dir &&
		yp.Mode == other.Mode &&
		yp.Copy == other.Copy &&
		yp.Text == other.Text &&
		yp.Symlink == other.Symlink &&
		yp.Mutable == other.Mutable)
}

type yamlArch struct {
	List []string
}

func (ya *yamlArch) UnmarshalYAML(value *yaml.Node) error {
	var s string
	var l []string
	if value.Decode(&s) == nil {
		ya.List = []string{s}
	} else if value.Decode(&l) == nil {
		ya.List = l
	} else {
		return fmt.Errorf("cannot decode arch")
	}
	// Validate arch correctness later for a better error message.
	return nil
}

func (ya yamlArch) MarshalYAML() (interface{}, error) {
	if len(ya.List) == 1 {
		return ya.List[0], nil
	}
	return ya.List, nil
}

var _ yaml.Marshaler = yamlArch{}

type yamlMode uint

func (ym yamlMode) MarshalYAML() (interface{}, error) {
	// Workaround for marshalling integers in octal format.
	// Ref: https://github.com/go-yaml/yaml/issues/420.
	node := &yaml.Node{}
	err := node.Encode(uint(ym))
	if err != nil {
		return nil, err
	}
	node.Value = fmt.Sprintf("0%o", ym)
	return node, nil
}

var _ yaml.Marshaler = yamlMode(0)

type yamlSlice struct {
	Essential []string             `yaml:"essential,omitempty"`
	Contents  map[string]*yamlPath `yaml:"contents,omitempty"`
	Mutate    string               `yaml:"mutate,omitempty"`
}

type yamlPubKey struct {
	ID    string `yaml:"id"`
	Armor string `yaml:"armor"`
}

var ubuntuAdjectives = map[string]string{
	"18.04": "bionic",
	"20.04": "focal",
	"22.04": "jammy",
	"22.10": "kinetic",
}

func parseRelease(baseDir, filePath string, data []byte) (*Release, error) {
	release := &Release{
		Path:     baseDir,
		Packages: make(map[string]*Package),
		Archives: make(map[string]*Archive),
	}

	fileName := stripBase(baseDir, filePath)

	yamlVar := yamlRelease{}
	dec := yaml.NewDecoder(bytes.NewBuffer(data))
	dec.KnownFields(false)
	err := dec.Decode(&yamlVar)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot parse release definition: %v", fileName, err)
	}
	if yamlVar.Format != "chisel-v1" && yamlVar.Format != "v1" {
		return nil, fmt.Errorf("%s: unknown format %q", fileName, yamlVar.Format)
	}
	// If format is "chisel-v1" we have to translate from the yaml key "v1-public-keys" to
	// "public-keys".
	if yamlVar.Format == "chisel-v1" {
		yamlVar.PubKeys = yamlVar.V1PubKeys
		for name, details := range yamlVar.Archives {
			details.PubKeys = details.V1PubKeys
			yamlVar.Archives[name] = details
		}
	}
	if len(yamlVar.Archives) == 0 {
		return nil, fmt.Errorf("%s: no archives defined", fileName)
	}

	// Decode the public keys and match against provided IDs.
	pubKeys := make(map[string]*packet.PublicKey, len(yamlVar.PubKeys))
	for keyName, yamlPubKey := range yamlVar.PubKeys {
		key, err := pgputil.DecodePubKey([]byte(yamlPubKey.Armor))
		if err != nil {
			return nil, fmt.Errorf("%s: cannot decode public key %q: %w", fileName, keyName, err)
		}
		if yamlPubKey.ID != key.KeyIdString() {
			return nil, fmt.Errorf("%s: public key %q armor has incorrect ID: expected %q, got %q", fileName, keyName, yamlPubKey.ID, key.KeyIdString())
		}
		pubKeys[keyName] = key
	}

	for archiveName, details := range yamlVar.Archives {
		if details.Version == "" {
			return nil, fmt.Errorf("%s: archive %q missing version field", fileName, archiveName)
		}
		if len(details.Suites) == 0 {
			adjective := ubuntuAdjectives[details.Version]
			if adjective == "" {
				return nil, fmt.Errorf("%s: archive %q missing suites field", fileName, archiveName)
			}
			details.Suites = []string{adjective}
		}
		if len(details.Components) == 0 {
			return nil, fmt.Errorf("%s: archive %q missing components field", fileName, archiveName)
		}
		if len(yamlVar.Archives) == 1 {
			details.Default = true
		} else if details.Default && release.DefaultArchive != "" {
			return nil, fmt.Errorf("%s: more than one default archive: %s, %s", fileName, release.DefaultArchive, archiveName)
		}
		if details.Default {
			release.DefaultArchive = archiveName
		}
		if len(details.PubKeys) == 0 {
			if yamlVar.Format == "chisel-v1" {
				return nil, fmt.Errorf("%s: archive %q missing v1-public-keys field", fileName, archiveName)
			} else {
				return nil, fmt.Errorf("%s: archive %q missing public-keys field", fileName, archiveName)
			}
		}
		var archiveKeys []*packet.PublicKey
		for _, keyName := range details.PubKeys {
			key, ok := pubKeys[keyName]
			if !ok {
				return nil, fmt.Errorf("%s: archive %q refers to undefined public key %q", fileName, archiveName, keyName)
			}
			archiveKeys = append(archiveKeys, key)
		}
		release.Archives[archiveName] = &Archive{
			Name:       archiveName,
			Version:    details.Version,
			Suites:     details.Suites,
			Components: details.Components,
			PubKeys:    archiveKeys,
		}
	}

	return release, err
}

func parsePackage(baseDir, pkgName, pkgPath string, data []byte) (*Package, error) {
	pkg := Package{
		Name:   pkgName,
		Path:   pkgPath,
		Slices: make(map[string]*Slice),
	}

	yamlPkg := yamlPackage{}
	dec := yaml.NewDecoder(bytes.NewBuffer(data))
	dec.KnownFields(false)
	err := dec.Decode(&yamlPkg)
	if err != nil {
		return nil, fmt.Errorf("cannot parse package %q slice definitions: %v", pkgName, err)
	}
	if yamlPkg.Name != pkg.Name {
		return nil, fmt.Errorf("%s: filename and 'package' field (%q) disagree", pkgPath, yamlPkg.Name)
	}
	pkg.Archive = yamlPkg.Archive

	zeroPath := yamlPath{}
	for sliceName, yamlSlice := range yamlPkg.Slices {
		match := snameExp.FindStringSubmatch(sliceName)
		if match == nil {
			return nil, fmt.Errorf("invalid slice name %q in %s", sliceName, pkgPath)
		}

		slice := &Slice{
			Package: pkgName,
			Name:    sliceName,
			Scripts: SliceScripts{
				Mutate: yamlSlice.Mutate,
			},
		}
		for _, refName := range yamlPkg.Essential {
			sliceKey, err := ParseSliceKey(refName)
			if err != nil {
				return nil, fmt.Errorf("package %q has invalid essential slice reference: %q", pkgName, refName)
			}
			if sliceKey.Package == slice.Package && sliceKey.Slice == slice.Name {
				// Do not add the slice to its own essentials list.
				continue
			}
			if slices.Contains(slice.Essential, sliceKey) {
				return nil, fmt.Errorf("package %s defined with redundant essential slice: %s", pkgName, refName)
			}
			slice.Essential = append(slice.Essential, sliceKey)
		}
		for _, refName := range yamlSlice.Essential {
			sliceKey, err := ParseSliceKey(refName)
			if err != nil {
				return nil, fmt.Errorf("package %q has invalid essential slice reference: %q", pkgName, refName)
			}
			if sliceKey.Package == slice.Package && sliceKey.Slice == slice.Name {
				return nil, fmt.Errorf("cannot add slice to itself as essential %q in %s", refName, pkgPath)
			}
			if slices.Contains(slice.Essential, sliceKey) {
				return nil, fmt.Errorf("slice %s defined with redundant essential slice: %s", slice, refName)
			}
			slice.Essential = append(slice.Essential, sliceKey)
		}

		if len(yamlSlice.Contents) > 0 {
			slice.Contents = make(map[string]PathInfo, len(yamlSlice.Contents))
		}
		for contPath, yamlPath := range yamlSlice.Contents {
			isDir := strings.HasSuffix(contPath, "/")
			comparePath := contPath
			if isDir {
				comparePath = comparePath[:len(comparePath)-1]
			}
			if !path.IsAbs(contPath) || path.Clean(contPath) != comparePath {
				return nil, fmt.Errorf("slice %s_%s has invalid content path: %s", pkgName, sliceName, contPath)
			}
			var kinds = make([]PathKind, 0, 3)
			var info string
			var mode uint
			var mutable bool
			var until PathUntil
			var arch []string
			var generate GenerateKind
			if yamlPath != nil && yamlPath.Generate != "" {
				zeroPathGenerate := zeroPath
				zeroPathGenerate.Generate = yamlPath.Generate
				if !yamlPath.SameContent(&zeroPathGenerate) || yamlPath.Until != UntilNone {
					return nil, fmt.Errorf("slice %s_%s path %s has invalid generate options",
						pkgName, sliceName, contPath)
				}
				if _, err := validateGeneratePath(contPath); err != nil {
					return nil, fmt.Errorf("slice %s_%s has invalid generate path: %s", pkgName, sliceName, err)
				}
				kinds = append(kinds, GeneratePath)
			} else if strings.ContainsAny(contPath, "*?") {
				if yamlPath != nil {
					if !yamlPath.SameContent(&zeroPath) {
						return nil, fmt.Errorf("slice %s_%s path %s has invalid wildcard options",
							pkgName, sliceName, contPath)
					}
				}
				kinds = append(kinds, GlobPath)
			}
			if yamlPath != nil {
				mode = uint(yamlPath.Mode)
				mutable = yamlPath.Mutable
				generate = yamlPath.Generate
				if yamlPath.Dir {
					if !strings.HasSuffix(contPath, "/") {
						return nil, fmt.Errorf("slice %s_%s path %s must end in / for 'make' to be valid",
							pkgName, sliceName, contPath)
					}
					kinds = append(kinds, DirPath)
				}
				if yamlPath.Text != nil {
					kinds = append(kinds, TextPath)
					info = *yamlPath.Text
				}
				if len(yamlPath.Symlink) > 0 {
					kinds = append(kinds, SymlinkPath)
					info = yamlPath.Symlink
				}
				if len(yamlPath.Copy) > 0 {
					kinds = append(kinds, CopyPath)
					info = yamlPath.Copy
					if info == contPath {
						info = ""
					}
				}
				until = yamlPath.Until
				switch until {
				case UntilNone, UntilMutate:
				default:
					return nil, fmt.Errorf("slice %s_%s has invalid 'until' for path %s: %q", pkgName, sliceName, contPath, until)
				}
				arch = yamlPath.Arch.List
				for _, s := range arch {
					if deb.ValidateArch(s) != nil {
						return nil, fmt.Errorf("slice %s_%s has invalid 'arch' for path %s: %q", pkgName, sliceName, contPath, s)
					}
				}
			}
			if len(kinds) == 0 {
				kinds = append(kinds, CopyPath)
			}
			if len(kinds) != 1 {
				list := make([]string, len(kinds))
				for i, s := range kinds {
					list[i] = string(s)
				}
				return nil, fmt.Errorf("conflict in slice %s_%s definition for path %s: %s", pkgName, sliceName, contPath, strings.Join(list, ", "))
			}
			if mutable && kinds[0] != TextPath && (kinds[0] != CopyPath || isDir) {
				return nil, fmt.Errorf("slice %s_%s mutable is not a regular file: %s", pkgName, sliceName, contPath)
			}
			slice.Contents[contPath] = PathInfo{
				Kind:     kinds[0],
				Info:     info,
				Mode:     mode,
				Mutable:  mutable,
				Until:    until,
				Arch:     arch,
				Generate: generate,
			}
		}

		pkg.Slices[sliceName] = slice
	}

	return &pkg, err
}

// validateGeneratePath validates that the path follows the following format:
//   - /slashed/path/to/dir/**
//
// Wildcard characters can only appear at the end as **, and the path before
// those wildcards must be a directory.
func validateGeneratePath(path string) (string, error) {
	if !strings.HasSuffix(path, "/**") {
		return "", fmt.Errorf("%s does not end with /**", path)
	}
	dirPath := strings.TrimSuffix(path, "**")
	if strings.ContainsAny(dirPath, "*?") {
		return "", fmt.Errorf("%s contains wildcard characters in addition to trailing **", path)
	}
	return dirPath, nil
}

func stripBase(baseDir, path string) string {
	// Paths must be clean for this to work correctly.
	return strings.TrimPrefix(path, baseDir+string(filepath.Separator))
}

func Select(release *Release, slices []SliceKey) (*Selection, error) {
	logf("Selecting slices...")

	selection := &Selection{
		Release: release,
	}

	sorted, err := order(release.Packages, slices)
	if err != nil {
		return nil, err
	}
	selection.Slices = make([]*Slice, len(sorted))
	for i, key := range sorted {
		selection.Slices[i] = release.Packages[key.Package].Slices[key.Slice]
	}

	paths := make(map[string]*Slice)
	for _, new := range selection.Slices {
		for newPath, newInfo := range new.Contents {
			if old, ok := paths[newPath]; ok {
				oldInfo := old.Contents[newPath]
				if !newInfo.SameContent(&oldInfo) || (newInfo.Kind == CopyPath || newInfo.Kind == GlobPath) && new.Package != old.Package {
					if old.Package > new.Package || old.Package == new.Package && old.Name > new.Name {
						old, new = new, old
					}
					return nil, fmt.Errorf("slices %s and %s conflict on %s", old, new, newPath)
				}
			} else {
				paths[newPath] = new
			}
			// An invalid "generate" value should only throw an error if that
			// particular slice is selected. Hence, the check is here.
			switch newInfo.Generate {
			case GenerateNone, GenerateManifest:
			default:
				return nil, fmt.Errorf("slice %s has invalid 'generate' for path %s: %q, consider an update if available",
					new, newPath, newInfo.Generate)
			}
		}
	}

	return selection, nil
}

// pathInfoToYAML converts a PathInfo object to a yamlPath object.
// The returned object takes pointers to the given PathInfo object.
func pathInfoToYAML(pi *PathInfo) (*yamlPath, error) {
	path := &yamlPath{
		Mode:    yamlMode(pi.Mode),
		Mutable: pi.Mutable,
		Until:   pi.Until,
		Arch:    yamlArch{List: pi.Arch},
	}
	switch pi.Kind {
	case DirPath:
		path.Dir = true
	case CopyPath:
		path.Copy = pi.Info
	case TextPath:
		path.Text = &pi.Info
	case SymlinkPath:
		path.Symlink = pi.Info
	case GlobPath:
		// Nothing more needs to be done for this type.
	default:
		return nil, fmt.Errorf("internal error: unrecognised PathInfo type: %s", pi.Kind)
	}
	return path, nil
}

// sliceToYAML converts a Slice object to a yamlSlice object.
func sliceToYAML(s *Slice) (*yamlSlice, error) {
	slice := &yamlSlice{
		Essential: make([]string, 0, len(s.Essential)),
		Contents:  make(map[string]*yamlPath, len(s.Contents)),
		Mutate:    s.Scripts.Mutate,
	}
	for _, key := range s.Essential {
		slice.Essential = append(slice.Essential, key.String())
	}
	for path, info := range s.Contents {
		// TODO remove the following line after upgrading to Go 1.22 or higher.
		info := info
		yamlPath, err := pathInfoToYAML(&info)
		if err != nil {
			return nil, err
		}
		slice.Contents[path] = yamlPath
	}
	return slice, nil
}

// packageToYAML converts a Package object to a yamlPackage object.
func packageToYAML(p *Package) (*yamlPackage, error) {
	pkg := &yamlPackage{
		Name:    p.Name,
		Archive: p.Archive,
		Slices:  make(map[string]yamlSlice, len(p.Slices)),
	}
	for name, slice := range p.Slices {
		yamlSlice, err := sliceToYAML(slice)
		if err != nil {
			return nil, err
		}
		pkg.Slices[name] = *yamlSlice
	}
	return pkg, nil
}
