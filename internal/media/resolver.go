package media

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var ErrInvalidLocator = errors.New("invalid media locator")

type Root struct {
	ID   string
	Path string
}

type Resolver struct {
	mu    sync.RWMutex
	roots map[string]string
}

// NewResolver builds a resolver and fails if any root is unusable.
func NewResolver(roots []Root) (*Resolver, error) {
	if len(roots) == 0 {
		return nil, errors.New("at least one media root is required")
	}
	resolved, err := resolveRoots(roots)
	if err != nil {
		return nil, err
	}
	return &Resolver{roots: resolved}, nil
}

// NewEmptyResolver returns a resolver with no roots; every locator is invalid
// until Update provides some.
func NewEmptyResolver() *Resolver {
	return &Resolver{roots: map[string]string{}}
}

// Update replaces the roots. Unusable roots are skipped and reported in the
// returned error, so one missing folder does not hide the others.
func (r *Resolver) Update(roots []Root) error {
	resolved := make(map[string]string, len(roots))
	var problems []error
	for _, root := range roots {
		one, err := resolveRoots([]Root{root})
		if err != nil {
			problems = append(problems, err)
			continue
		}
		for id, path := range one {
			resolved[id] = path
		}
	}
	r.mu.Lock()
	r.roots = resolved
	r.mu.Unlock()
	return errors.Join(problems...)
}

func resolveRoots(roots []Root) (map[string]string, error) {
	resolved := make(map[string]string, len(roots))
	for _, root := range roots {
		if root.ID == "" || root.Path == "" {
			return nil, errors.New("media root ID and path are required")
		}
		if _, exists := resolved[root.ID]; exists {
			return nil, fmt.Errorf("duplicate media root ID %q", root.ID)
		}
		absolute, err := filepath.Abs(root.Path)
		if err != nil {
			return nil, fmt.Errorf("resolve media root %q: %w", root.ID, err)
		}
		canonical, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, fmt.Errorf("resolve media root %q: %w", root.ID, err)
		}
		info, err := os.Stat(canonical)
		if err != nil {
			return nil, fmt.Errorf("inspect media root %q: %w", root.ID, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("media root %q is not a directory", root.ID)
		}
		resolved[root.ID] = filepath.Clean(canonical)
	}
	return resolved, nil
}

// Resolve returns the canonical path of a regular file inside a configured
// root. It is used by consumers that must hand a path to an external process.
func (r *Resolver) Resolve(rootID, relativePath string) (string, error) {
	canonical, err := r.canonicalPath(rootID, relativePath)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", os.ErrNotExist
		}
		return "", fmt.Errorf("inspect media file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", ErrInvalidLocator
	}
	return canonical, nil
}

func (r *Resolver) canonicalPath(rootID, relativePath string) (string, error) {
	r.mu.RLock()
	root, ok := r.roots[rootID]
	r.mu.RUnlock()
	if !ok || !isSafeRelativePath(relativePath) {
		return "", ErrInvalidLocator
	}

	candidate := filepath.Join(root, filepath.FromSlash(relativePath))
	canonical, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", os.ErrNotExist
		}
		return "", ErrInvalidLocator
	}
	rel, err := filepath.Rel(root, canonical)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrInvalidLocator
	}
	return canonical, nil
}

func (r *Resolver) Open(rootID, relativePath string) (*os.File, error) {
	canonical, err := r.canonicalPath(rootID, relativePath)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(canonical)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("open media file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect media file: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, ErrInvalidLocator
	}
	return file, nil
}

func isSafeRelativePath(value string) bool {
	if value == "" || strings.ContainsRune(value, '\x00') ||
		strings.Contains(value, `\`) || strings.Contains(value, ":") {
		return false
	}
	localPath := filepath.FromSlash(value)
	if !filepath.IsLocal(localPath) || filepath.IsAbs(localPath) {
		return false
	}
	if filepath.VolumeName(localPath) != "" {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
