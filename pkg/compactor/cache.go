package compactor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mmornati/leanproxy-mcp/internal/cachefile"
)

type Cache interface {
	Get(ctx context.Context, serverName string, originalHash string) (*DistilledManifest, error)
	Set(ctx context.Context, serverName string, manifest *DistilledManifest) error
	Invalidate(ctx context.Context, serverName string) error
}

type FileCache struct {
	cacheDir string
	logger   *slog.Logger
	mu       sync.RWMutex
	inMemory map[string]*DistilledManifest
}

func NewFileCache(cacheDir string, logger *slog.Logger) (*FileCache, error) {
	if logger == nil {
		logger = slog.Default()
	}

	if cacheDir == "" {
		dir, err := cachefile.Dir("distilled")
		if err != nil {
			return nil, fmt.Errorf("compactor: %w", err)
		}
		cacheDir = dir
	} else if err := os.MkdirAll(cacheDir, cachefile.DirPerm); err != nil {
		return nil, fmt.Errorf("compactor: create cache dir: %w", err)
	}

	if removed, err := cachefile.SweepTemp(cacheDir); err != nil {
		logger.Debug("compactor: could not sweep abandoned temp files", "error", err)
	} else if removed > 0 {
		logger.Debug("compactor: swept abandoned temp files", "count", removed)
	}

	return &FileCache{
		cacheDir: cacheDir,
		logger:   logger,
		inMemory: make(map[string]*DistilledManifest),
	}, nil
}

func (c *FileCache) cacheKey(serverName, originalHash string) string {
	hash := sha256.Sum256([]byte(serverName + originalHash))
	return fmt.Sprintf("%x", hash)
}

// keyLen is how much of the hex cache key goes into a filename. Invalidate
// relies on this being a fixed width to tell a key apart from a longer server
// name that happens to share a prefix.
const keyLen = 16

// filePrefix is the shared leading component of every cache file for a
// server. Invalidate matches on it, so it must stay in step with filePath.
//
// Sanitizing is many-to-one, so this prefix alone does not identify a server:
// "a b" and "a_b" both yield "a_b_". Invalidate confirms the owner by reading
// each candidate rather than trusting the name.
func (c *FileCache) filePrefix(serverName string) string {
	return cachefile.SanitizeName(serverName) + "_"
}

func (c *FileCache) filePath(serverName string, originalHash string) string {
	key := c.cacheKey(serverName, originalHash)
	return filepath.Join(c.cacheDir, c.filePrefix(serverName)+key[:keyLen]+".json")
}

// memKey identifies an in-memory entry. The NUL separator keeps a server name
// from running into the hash: with bare concatenation, sweeping the prefix
// "git" also matched the entry for "github-server".
func memKey(serverName, originalHash string) string {
	return serverName + "\x00" + originalHash
}

// isKeyComponent reports whether s is exactly the hex key filePath appends,
// which is what distinguishes "foo_<key>.json" (server "foo") from
// "foo_bar_<key>.json" (server "foo_bar") when matching the prefix "foo_".
func isKeyComponent(s string) bool {
	if len(s) != keyLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (c *FileCache) Get(ctx context.Context, serverName string, originalHash string) (*DistilledManifest, error) {
	c.mu.RLock()
	if cached, ok := c.inMemory[serverName+originalHash]; ok {
		c.mu.RUnlock()
		c.logger.Debug("cache hit (memory)", "server", serverName)
		return cached, nil
	}
	c.mu.RUnlock()

	filePath := c.filePath(serverName, originalHash)
	data, err := os.ReadFile(filePath) // #nosec G304 -- internal cache path derived from server name
	if err != nil {
		if os.IsNotExist(err) {
			c.logger.Debug("cache miss", "server", serverName)
			return nil, nil
		}
		return nil, fmt.Errorf("compactor: read cache file: %w", err)
	}

	var distilled DistilledManifest
	if err := json.Unmarshal(data, &distilled); err != nil {
		return nil, fmt.Errorf("compactor: unmarshal cached manifest: %w", err)
	}

	c.mu.Lock()
	c.inMemory[memKey(serverName, originalHash)] = &distilled
	c.mu.Unlock()

	c.logger.Debug("cache hit (disk)", "server", serverName)
	return &distilled, nil
}

func (c *FileCache) Set(ctx context.Context, serverName string, manifest *DistilledManifest) error {
	c.mu.Lock()
	c.inMemory[memKey(serverName, manifest.OriginalHash)] = manifest
	c.mu.Unlock()

	filePath := c.filePath(serverName, manifest.OriginalHash)
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("compactor: marshal manifest for cache: %w", err)
	}

	if err := cachefile.WriteAtomic(filePath, data, cachefile.FilePerm); err != nil {
		return fmt.Errorf("compactor: write cache file: %w", err)
	}

	c.logger.Debug("cached distilled manifest", "server", serverName, "path", filePath)
	return nil
}

// Invalidate drops every entry belonging to serverName, in memory and on disk.
// Both halves key off the raw name: matching the sanitized name on disk alone
// would also take out a different server that sanitizes to the same prefix.
func (c *FileCache) Invalidate(ctx context.Context, serverName string) error {
	c.mu.Lock()
	prefix := memKey(serverName, "")
	for key := range c.inMemory {
		if strings.HasPrefix(key, prefix) {
			delete(c.inMemory, key)
		}
	}
	c.mu.Unlock()

	paths, err := c.filesFor(serverName)
	if err != nil {
		return err
	}

	removed := 0
	var errs []error
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			// Keep going so one unremovable file does not strand the rest,
			// but report: the sibling toolstore cache propagates this, and a
			// silent nil here tells `compactor rebuild` it succeeded.
			c.logger.Warn("failed to remove cache file", "path", path, "error", err)
			errs = append(errs, err)
			continue
		}
		removed++
	}

	c.logger.Debug("invalidated cache", "server", serverName, "files_removed", removed)
	if len(errs) > 0 {
		return fmt.Errorf("compactor: remove cache files: %w", errors.Join(errs...))
	}
	return nil
}

// filesFor returns the cache files owned by serverName. A filename only
// narrows the candidates -- the sanitized prefix is ambiguous and the appended
// key is a hash of the raw name it cannot be reversed from -- so ownership is
// settled by reading the manifest's own ServerName.
func (c *FileCache) filesFor(serverName string) ([]string, error) {
	entries, err := os.ReadDir(c.cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("compactor: read cache dir: %w", err)
	}

	prefix := c.filePrefix(serverName)
	var paths []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		key, ok := strings.CutSuffix(rest, ".json")
		if !ok || !isKeyComponent(key) {
			// "foo_bar_<key>.json" survives the prefix "foo_" but leaves
			// "bar_<key>", which is not a bare key: a different server.
			continue
		}

		path := filepath.Join(c.cacheDir, name)
		owner, err := manifestOwner(path)
		if err != nil {
			// Unreadable or malformed: nothing can serve it, and its name is
			// in this server's shape, so clear it rather than leak it.
			c.logger.Debug("removing unreadable cache file", "path", path, "error", err)
			paths = append(paths, path)
			continue
		}
		if owner == serverName {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

func manifestOwner(path string) (string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is built from c.cacheDir entries
	if err != nil {
		return "", err
	}
	var m struct {
		ServerName string `json:"server_name"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return "", err
	}
	return m.ServerName, nil
}

type NoOpCache struct{}

func NewNoOpCache() *NoOpCache {
	return &NoOpCache{}
}

func (c *NoOpCache) Get(ctx context.Context, serverName string, originalHash string) (*DistilledManifest, error) {
	return nil, nil
}

func (c *NoOpCache) Set(ctx context.Context, serverName string, manifest *DistilledManifest) error {
	return nil
}

func (c *NoOpCache) Invalidate(ctx context.Context, serverName string) error {
	return nil
}

func hashForInvalidation(manifest RawManifest) string {
	data, _ := json.Marshal(manifest)
	hash := sha256.Sum256(data)
	return fmt.Sprintf("%x", hash)
}

var _ Cache = (*FileCache)(nil)
var _ Cache = (*NoOpCache)(nil)

func isCacheValid(cached *DistilledManifest, currentHash string) bool {
	if cached == nil {
		return false
	}
	if cached.OriginalHash != currentHash {
		return false
	}
	if time.Since(cached.DistilledAt) > 24*time.Hour {
		return false
	}
	return true
}
