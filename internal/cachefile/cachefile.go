// Package cachefile holds the on-disk primitives shared by LeanProxy's
// file-backed caches (pkg/toolstore, pkg/compactor): where cache files live,
// what they may be named, and how they are replaced without leaving a torn
// file behind. The caches keep their own domain-shaped APIs on top; only the
// filesystem mechanics live here, so a fix like atomic replacement lands in
// one place instead of two.
package cachefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Root is the per-user directory holding every LeanProxy cache subdirectory.
const Root = ".config/leanproxy"

// DirPerm is the mode for cache directories: owner-only, since cached tool
// manifests can carry server descriptions the user has not chosen to share.
const DirPerm os.FileMode = 0700

// FilePerm is the mode for cache files, owner-only for the same reason.
const FilePerm os.FileMode = 0600

// tempPrefix leads every temp file WriteAtomic creates. It deliberately does
// not embed the target's name: os.CreateTemp appends up to 10 digits, so a
// name-derived pattern cost len(target)+15 bytes and pushed long-but-legal
// targets past NAME_MAX (255), turning writes that plain os.WriteFile still
// accepted into hard failures. A constant prefix also gives SweepTemp
// something unambiguous to match.
const tempPrefix = ".leanproxy-tmp"

// tempMaxAge is how long a temp file must have gone untouched before SweepTemp
// treats it as abandoned. Writes complete in well under a millisecond, so this
// is a wide margin against reaping a concurrent writer's file in flight.
const tempMaxAge = time.Hour

// Dir returns the cache subdirectory named sub under the user's LeanProxy
// config root, creating it if needed. Callers wrap the error with their own
// package prefix.
//
// Home comes from os.UserHomeDir, which reads $HOME, rather than user.Current,
// which reads the passwd database and ignores it. Release binaries are built
// CGO_ENABLED=0, so os/user takes its pure-Go path and fails for a UID with no
// passwd entry — `docker run --user 1000:1000` against a distroless image, for
// instance. Both callers downgrade that failure to a warning and fall back to
// a no-op cache, so the process would silently stop caching entirely.
func Dir(sub string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get user home dir: %w", err)
	}
	return DirUnder(home, sub)
}

// DirUnder is Dir with an explicit home directory, so tests can exercise the
// path layout without touching the developer's real home.
func DirUnder(home, sub string) (string, error) {
	dir := filepath.Join(home, Root, sub)
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	return dir, nil
}

// SweepTemp removes temp files abandoned in dir by a WriteAtomic that never
// reached its rename — a SIGKILL, OOM kill, or power loss in between. The
// deferred cleanup inside WriteAtomic only covers an in-process error return,
// and nothing else reaps these: the caches enumerate "*.json" and invalidate
// by name, so a leading-dot temp is invisible to `cache --clear` and
// `compactor rebuild` alike. leanproxy runs as a stdio daemon that clients
// terminate by killing the process, so without this they accumulate forever.
//
// Only files untouched for at least tempMaxAge are removed, leaving a
// concurrent writer's in-flight temp alone. Callers treat failure as
// non-fatal: a cache that cannot tidy up still works.
func SweepTemp(dir string) (removed int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read cache dir: %w", err)
	}

	cutoff := time.Now().Add(-tempMaxAge)
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), tempPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue // vanished under us; nothing to do
		}
		if info.ModTime().After(cutoff) {
			continue // possibly a write in flight
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, err)
			continue
		}
		removed++
	}
	if len(errs) > 0 {
		return removed, fmt.Errorf("remove temp files: %w", errors.Join(errs...))
	}
	return removed, nil
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
//
// It deliberately does not fsync. Surviving power loss would need
// write -> fsync(file) -> rename -> fsync(dir), and on darwin File.Sync issues
// F_FULLFSYNC, a full drive barrier: measured on a 29KB manifest that is
// ~4.7ms per write against ~140us without, and callers persist one file per
// configured server before the listener opens. A cache whose contents are
// re-fetched on a bad read does not need that trade; the rename alone provides
// the visibility guarantee above.
func WriteAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, tempPrefix+"*")
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
	if err = f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}
