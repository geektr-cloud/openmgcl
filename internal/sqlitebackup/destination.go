// Package sqlitebackup implements the reusable SQLite backup layer used by
// mgcl write sessions. It validates configured destinations, performs startup
// probes and initial backups, runs the dirty-merge worker for sqlite targets,
// and executes the shutdown drain, checkpoint, and path copy sequence.
package sqlitebackup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Scheme is a backup destination scheme. The two schemes have deliberately
// different semantics and cannot be substituted for one another.
type Scheme string

const (
	SchemeSQLite Scheme = "sqlite"
	SchemePath   Scheme = "path"
)

// Destination is one validated, normalized backup target.
type Destination struct {
	Scheme Scheme
	Path   string
}

func (d Destination) String() string { return string(d.Scheme) + ":" + d.Path }

// IdentityPaths returns the destination path and its SQLite sidecar paths.
func (d Destination) IdentityPaths() []string { return identityPaths(d.Path) }

// ParseDestinations parses and normalizes configured targets. Disjoint specs
// may be relative to baseDir; an empty baseDir means the process working
// directory. It rejects malformed schemes/paths and conflicting destinations.
func ParseDestinations(specs []string, baseDir string) ([]Destination, error) {
	if len(specs) == 0 {
		return nil, nil
	}

	out := make([]Destination, 0, len(specs))
	for i, spec := range specs {
		scheme, rawPath, ok := strings.Cut(spec, ":")
		if !ok {
			return nil, fmt.Errorf("backup_to[%d]: missing scheme in %q", i, spec)
		}
		if scheme == "" {
			return nil, fmt.Errorf("backup_to[%d]: empty scheme in %q", i, spec)
		}
		switch Scheme(scheme) {
		case SchemeSQLite, SchemePath:
		default:
			return nil, fmt.Errorf("backup_to[%d]: unsupported scheme %q", i, scheme)
		}
		if rawPath == "" {
			return nil, fmt.Errorf("backup_to[%d]: empty path", i)
		}
		if strings.IndexByte(rawPath, 0) >= 0 {
			return nil, fmt.Errorf("backup_to[%d]: path contains NUL", i)
		}

		normalized, err := normalizeDestinationPath(rawPath, baseDir)
		if err != nil {
			return nil, fmt.Errorf("backup_to[%d]: %w", i, err)
		}
		out = append(out, Destination{Scheme: Scheme(scheme), Path: normalized})
	}

	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if destinationsConflict(out[i], out[j]) {
				return nil, fmt.Errorf("backup destinations conflict: %s and %s", out[i], out[j])
			}
		}
	}
	return out, nil
}

func normalizeDestinationPath(rawPath, baseDir string) (string, error) {
	path := rawPath
	if !filepath.IsAbs(path) {
		if baseDir == "" {
			baseDir = "."
		}
		path = filepath.Join(baseDir, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", rawPath, err)
	}

	fi, err := os.Lstat(abs)
	switch {
	case err == nil:
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("destination %q is a symlink", abs)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", fmt.Errorf("resolve destination %q: %w", abs, err)
		}
		return real, nil
	case os.IsNotExist(err):
		parent := filepath.Dir(abs)
		realParent, err := filepath.EvalSymlinks(parent)
		if err != nil {
			return "", fmt.Errorf("destination %q parent directory: %w", abs, err)
		}
		return filepath.Join(realParent, filepath.Base(abs)), nil
	default:
		return "", fmt.Errorf("inspect destination %q: %w", abs, err)
	}
}

func destinationsConflict(a, b Destination) bool {
	for _, pa := range a.IdentityPaths() {
		for _, pb := range b.IdentityPaths() {
			if pa == pb {
				return true
			}
		}
	}
	return false
}

// destinationsSameFile detects two differently named destinations that still
// refer to the same file, for example through hard links. ParseDestinations
// catches normalized path/sidecar conflicts; New calls this to cover filesystem
// identity as well.
func destinationsSameFile(a, b Destination) bool {
	for _, pa := range a.IdentityPaths() {
		fiA, err := os.Stat(pa)
		if err != nil {
			continue
		}
		for _, pb := range b.IdentityPaths() {
			fiB, err := os.Stat(pb)
			if err != nil {
				continue
			}
			if os.SameFile(fiA, fiB) {
				return true
			}
		}
	}
	return false
}

func identityPaths(path string) []string {
	return []string{path, path + "-wal", path + "-shm", path + "-journal"}
}

// sourceInfo describes the source database's path identity. It includes both
// the path used to open SQLite and the symlink-resolved path, because sidecar
// placement can differ when the database was opened through a symlink.
type sourceInfo struct {
	paths []string
	files []os.FileInfo
}

func newSourceInfo(path string) sourceInfo {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	paths := identityPaths(abs)
	if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
		paths = append(paths, identityPaths(real)...)
	}

	var files []os.FileInfo
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			files = append(files, fi)
		}
	}
	return sourceInfo{paths: paths, files: files}
}

func (s sourceInfo) conflictsWith(paths []string) bool {
	for _, p := range paths {
		for _, sp := range s.paths {
			if p == sp {
				return true
			}
		}
	}
	return false
}

func (s sourceInfo) sameFile(info os.FileInfo) bool {
	for _, sf := range s.files {
		if os.SameFile(info, sf) {
			return true
		}
	}
	return false
}

func validateDestination(d Destination, src sourceInfo) error {
	if src.conflictsWith(d.IdentityPaths()) {
		return errors.New("destination conflicts with the source database or its sidecars")
	}
	return validateTargetFile(d.Path, src, true)
}

// validateTargetFile applies the startup checks for one target. A missing
// target is allowed when its parent directory exists and is a directory.
func validateTargetFile(path string, src sourceInfo, deep bool) error {
	if err := checkTargetSidecars(path, src); err != nil {
		return err
	}

	fi, err := os.Lstat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspect target: %w", err)
		}
		return validateTargetParent(path)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("target is a symlink")
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("target is not a regular file (%s)", fi.Mode().Type())
	}
	if n, ok := linkCount(fi); ok && n > 1 {
		return fmt.Errorf("target has %d hard links", n)
	}
	if src.sameFile(fi) {
		return errors.New("target is the source database")
	}
	if fi.Size() > 0 {
		if err := checkSQLiteHeader(path); err != nil {
			return fmt.Errorf("target is not a usable SQLite database: %w", err)
		}
		if deep {
			if err := quickCheckSQLite(path); err != nil {
				return fmt.Errorf("target is not a valid SQLite database: %w", err)
			}
		}
	}
	return nil
}

func validateTargetParent(path string) error {
	parent := filepath.Dir(path)
	fi, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("target parent directory: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("target parent %q is not a directory", parent)
	}
	return nil
}

func checkTargetSidecars(path string, src sourceInfo) error {
	for _, p := range identityPaths(path)[1:] {
		fi, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("inspect target sidecar %q: %w", p, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("target sidecar %q is a symlink", p)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("target sidecar %q is not a regular file", p)
		}
		if n, ok := linkCount(fi); ok && n > 1 {
			return fmt.Errorf("target sidecar %q has %d hard links", p, n)
		}
		if src.sameFile(fi) {
			return fmt.Errorf("target sidecar %q is the source database", p)
		}
		if strings.HasSuffix(p, "-wal") || strings.HasSuffix(p, "-journal") {
			if fi.Size() > 0 {
				return fmt.Errorf("target sidecar %q is not empty", p)
			}
		}
	}
	return nil
}

// probePath verifies that the target directory supports create, write, fsync,
// close, and remove without touching the real target file.
func probePath(d Destination) error {
	dir := filepath.Dir(d.Path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(d.Path)+".mgcl-probe-*")
	if err != nil {
		return fmt.Errorf("probe directory %q: %w", dir, err)
	}
	name := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(name)
	}

	if _, err := f.Write([]byte("mgcl backup probe\n")); err != nil {
		cleanup()
		return fmt.Errorf("probe %q: write: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("probe %q: fsync: %w", name, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("probe %q: close: %w", name, err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("probe %q: remove: %w", name, err)
	}
	return nil
}
