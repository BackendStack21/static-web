package cache

import (
	"errors"
	"io/fs"
	"os"
)

// ErrNotInRoot is returned by OpenInRoot for a path that does not lie inside
// the given root.
var ErrNotInRoot = errors.New("cache: path is not inside the root")

// ErrRootChanged is returned by OpenInRoot when the root path no longer
// names a real directory: it was replaced by a symlink, or changed while it
// was being opened.
var ErrRootChanged = errors.New("cache: root directory was replaced")

// OpenInRoot opens p, a symlink-resolved path inside realRoot (or realRoot
// itself), through an os.Root for realRoot. Every component is opened
// relative to its parent and no symlink is followed out of the root, so a
// component replaced by a symlink after p was validated cannot lead outside
// realRoot: the open fails instead (IsEscape reports true for that error).
//
// The root is opened by path on every call rather than held open, so that a
// root directory replaced by a rename (a common deploy step) is followed the
// same way path-based access follows it. The opened root must be the real
// directory an Lstat of realRoot names: a root path replaced by a symlink
// (which needs write access to its parent) is refused, not followed.
//
// os.Root opens every directory it walks for reading, so the server user
// needs read permission (not only search permission) on each directory from
// realRoot down to p.
//
// os.Root still follows symlinks that stay inside the root. Callers that must
// also refuse a final component swapped for an in-root symlink compare the
// opened file with an Lstat of p (see readRegularFile).
func OpenInRoot(realRoot, p string) (*os.File, error) {
	rel := "."
	if p != realRoot {
		r, ok := relInsideRoot(realRoot, p)
		if !ok {
			return nil, &fs.PathError{Op: "open", Path: p, Err: ErrNotInRoot}
		}
		rel = r
	}
	r, err := os.OpenRoot(realRoot)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	opened, err := r.Stat(".")
	if err != nil {
		return nil, err
	}
	if named, err := os.Lstat(realRoot); err != nil || !named.IsDir() || !os.SameFile(named, opened) {
		return nil, &fs.PathError{Op: "open", Path: realRoot, Err: ErrRootChanged}
	}
	return r.Open(rel)
}

// IsEscape reports whether err is OpenInRoot refusing a path that leads
// outside the root: one not inside it to begin with, one whose components
// now resolve outside it (a symlink swapped in after validation, or an
// absolute symlink, which os.Root never follows), or a root path that was
// itself replaced by a symlink.
func IsEscape(err error) bool {
	if errors.Is(err, ErrNotInRoot) || errors.Is(err, ErrRootChanged) {
		return true
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		// os.Root's error for a component that leaves the root is unexported;
		// its text has been stable since Go 1.24. TestIsEscape pins it.
		return pe.Err != nil && pe.Err.Error() == "path escapes from parent"
	}
	return false
}
