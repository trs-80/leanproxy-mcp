// Package cachefile holds the on-disk primitives shared by LeanProxy's
// file-backed caches (pkg/toolstore, pkg/compactor): where cache files live,
// what they may be named, and how they are replaced without leaving a torn
// file behind. The caches keep their own domain-shaped APIs on top; only the
// filesystem mechanics live here, so a fix like atomic replacement lands in
// one place instead of two.
package cachefile

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
)

// Root is the per-user directory holding every LeanProxy cache subdirectory.
const Root = ".config/leanproxy"

// DirPerm is the mode for cache directories: owner-only, since cached tool
// manifests can carry server descriptions the user has not chosen to share.
const DirPerm os.FileMode = 0700

// FilePerm is the mode for cache files, owner-only for the same reason.
const FilePerm os.FileMode = 0600

// Dir returns the cache subdirectory named sub under the user's LeanProxy
// config root, creating it if needed. Callers wrap the error with their own
// package prefix.
func Dir(sub string) (string, error) {
	usr, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("get user home dir: %w", err)
	}
	dir := filepath.Join(usr.HomeDir, Root, sub)
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	return dir, nil
}

// SanitizeName maps an arbitrary server name onto a single safe filename
// component, replacing every character outside [A-Za-z0-9_-] with '_'. Server
// names come from user config, so this is defense in depth rather than a
// hostile-input boundary: it keeps a name containing '/' or ".." from steering
// a cache write outside its directory.
func SanitizeName(name string) string {
	result := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			result = append(result, c)
		} else {
			result = append(result, '_')
		}
	}
	return string(result)
}

// WriteAtomic writes data to path via a temp file in the same directory
// followed by a rename, so a reader either sees the previous contents or the
// complete new ones — never a half-written file from an interrupted write.
// The rename is atomic on Unix; on Windows os.Rename is not, which is
// acceptable here because a torn cache file is re-fetched rather than fatal.
func WriteAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()

	// os.CreateTemp already creates 0600; set perm explicitly so the caller's
	// intent survives a future change to that default.
	if err = f.Chmod(perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}
