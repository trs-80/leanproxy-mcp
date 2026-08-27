package compactor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileCache_SetAndGet(t *testing.T) {
	tmpDir := t.TempDir()

	cache, err := NewFileCache(tmpDir, nil)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	manifest := &DistilledManifest{
		ServerName:   "test-server",
		OriginalHash: "abc123",
		Tools: []DistilledTool{
			{Name: "tool1", Description: "Test tool", Parameters: json.RawMessage("{}")},
		},
		DistilledAt: time.Now(),
	}

	ctx := context.Background()

	if err := cache.Set(ctx, "test-server", manifest); err != nil {
		t.Fatalf("failed to set cache: %v", err)
	}

	retrieved, err := cache.Get(ctx, "test-server", "abc123")
	if err != nil {
		t.Fatalf("failed to get cache: %v", err)
	}

	if retrieved == nil {
		t.Fatal("expected retrieved manifest, got nil")
	}

	if retrieved.ServerName != "test-server" {
		t.Errorf("expected server_name 'test-server', got %s", retrieved.ServerName)
	}

	if len(retrieved.Tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(retrieved.Tools))
	}
}

// A server name with path separators must not steer the write out of the
// cache directory, and Invalidate must still find what Set wrote — the glob
// prefix and the filename have to agree on the sanitized name.
func TestFileCache_SanitizesServerName(t *testing.T) {
	tmpDir := t.TempDir()

	cache, err := NewFileCache(tmpDir, nil)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	const serverName = "../evil server"
	manifest := &DistilledManifest{
		ServerName:   serverName,
		OriginalHash: "abc123",
		Tools:        []DistilledTool{{Name: "tool1", Parameters: json.RawMessage("{}")}},
		DistilledAt:  time.Now(),
	}

	ctx := context.Background()
	if err := cache.Set(ctx, serverName, manifest); err != nil {
		t.Fatalf("failed to set cache: %v", err)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("failed to read cache dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 cache file, got %d", len(entries))
	}
	if !strings.HasPrefix(entries[0].Name(), "___evil_server_") {
		t.Errorf("cache file %q was not written under a sanitized name", entries[0].Name())
	}

	if err := cache.Invalidate(ctx, serverName); err != nil {
		t.Fatalf("failed to invalidate: %v", err)
	}

	entries, err = os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("failed to read cache dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected cache dir to be empty after invalidate, got %d files", len(entries))
	}
}

func distilled(name, hash string) *DistilledManifest {
	return &DistilledManifest{
		ServerName:   name,
		OriginalHash: hash,
		Tools:        []DistilledTool{{Name: "tool1", Parameters: json.RawMessage("{}")}},
		DistilledAt:  time.Now(),
	}
}

// Invalidate must take out exactly one server's files. Three ways it used not
// to: an undelimited glob prefix ("foo_*" swallowing foo_bar's files), the
// sanitizer aliasing distinct names onto one prefix, and the in-memory sweep
// matching a raw prefix so "git" also hit "github-server".
func TestFileCache_InvalidateOnlyTargetServer(t *testing.T) {
	tests := []struct {
		name    string
		servers []string
		target  string
	}{
		{"undelimited prefix", []string{"foo", "foo_bar"}, "foo"},
		{"sanitize aliasing", []string{"a b", "a_b"}, "a b"},
		{"raw prefix in memory", []string{"git", "github-server"}, "git"},
		{"separator in name", []string{"a/b", "a_b"}, "a/b"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			cache, err := NewFileCache(tmpDir, nil)
			if err != nil {
				t.Fatalf("failed to create cache: %v", err)
			}
			ctx := context.Background()

			for i, s := range tc.servers {
				hash := fmt.Sprintf("h%d", i)
				if err := cache.Set(ctx, s, distilled(s, hash)); err != nil {
					t.Fatalf("Set(%q): %v", s, err)
				}
			}

			if err := cache.Invalidate(ctx, tc.target); err != nil {
				t.Fatalf("Invalidate(%q): %v", tc.target, err)
			}

			for i, s := range tc.servers {
				hash := fmt.Sprintf("h%d", i)
				got, err := cache.Get(ctx, s, hash)
				if err != nil {
					t.Fatalf("Get(%q): %v", s, err)
				}
				if s == tc.target {
					if got != nil {
						t.Errorf("Get(%q) returned a manifest after Invalidate", s)
					}
					continue
				}
				if got == nil {
					t.Errorf("Invalidate(%q) also evicted bystander %q", tc.target, s)
				}
			}
		})
	}
}

// The in-memory and on-disk halves must evict the same set: previously the
// memory sweep matched raw names while the glob matched sanitized ones, so a
// manifest could survive in memory with its backing file already deleted.
func TestFileCache_InvalidateMemoryAndDiskAgree(t *testing.T) {
	tmpDir := t.TempDir()
	cache, err := NewFileCache(tmpDir, nil)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}
	ctx := context.Background()

	if err := cache.Set(ctx, "a/b", distilled("a/b", "h1")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Sanitizes to the same disk prefix as "a/b" but is a different server.
	if err := cache.Invalidate(ctx, "a b"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	got, err := cache.Get(ctx, "a/b", "h1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected a/b's file to survive, found %d files", len(entries))
	}
	if got == nil {
		t.Error("a/b was evicted from memory by an unrelated Invalidate")
	}
}

func TestFileCache_Get_NotFound(t *testing.T) {
	tmpDir := t.TempDir()

	cache, err := NewFileCache(tmpDir, nil)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	ctx := context.Background()

	retrieved, err := cache.Get(ctx, "nonexistent", "hash123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if retrieved != nil {
		t.Error("expected nil for nonexistent key")
	}
}

func TestFileCache_Invalidate(t *testing.T) {
	tmpDir := t.TempDir()

	cache, err := NewFileCache(tmpDir, nil)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	manifest := &DistilledManifest{
		ServerName:   "test-server",
		OriginalHash: "abc123",
		Tools:        []DistilledTool{},
		DistilledAt:  time.Now(),
	}

	ctx := context.Background()

	if err := cache.Set(ctx, "test-server", manifest); err != nil {
		t.Fatalf("failed to set cache: %v", err)
	}

	if err := cache.Invalidate(ctx, "test-server"); err != nil {
		t.Fatalf("failed to invalidate cache: %v", err)
	}

	retrieved, err := cache.Get(ctx, "test-server", "abc123")
	if err != nil {
		t.Fatalf("unexpected error after invalidation: %v", err)
	}

	if retrieved != nil {
		t.Error("expected nil after invalidation")
	}
}

func TestNoOpCache(t *testing.T) {
	cache := NewNoOpCache()

	ctx := context.Background()

	retrieved, err := cache.Get(ctx, "test", "hash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if retrieved != nil {
		t.Error("expected nil from NoOpCache.Get")
	}

	if err := cache.Set(ctx, "test", &DistilledManifest{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := cache.Invalidate(ctx, "test"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFileCache_DirectoryCreation(t *testing.T) {
	tmpDir := t.TempDir()
	cacheDir := filepath.Join(tmpDir, "nested", "path", "to", "cache")

	cache, err := NewFileCache(cacheDir, nil)
	if err != nil {
		t.Fatalf("failed to create cache: %v", err)
	}

	ctx := context.Background()

	manifest := &DistilledManifest{
		ServerName:   "test",
		OriginalHash: "hash",
		Tools:        []DistilledTool{},
		DistilledAt:  time.Now(),
	}

	if err := cache.Set(ctx, "test", manifest); err != nil {
		t.Fatalf("failed to set cache: %v", err)
	}

	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		t.Error("expected cache directory to be created")
	}
}
