package zvecdb

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// collectionName is what a collection may be called. The name arrives from a
// flow and becomes a path under the data directory, so it is validated rather
// than escaped: no separators, no parent references, no leading dot, nothing
// a shell or a filesystem reads specially.
//
// That is a security boundary, not tidiness. Without it a node could name
// "../../../etc" and the engine would happily open, and with a write action
// modify, a directory the profile never granted.
var collectionName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// CollectionPath resolves a collection name to its directory inside the
// configured data directory.
func (c Config) CollectionPath(name string) (string, error) {
	clean, err := CheckName(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(c.DataDir, clean), nil
}

// CheckName validates a collection name and returns it trimmed.
func CheckName(name string) (string, error) {
	clean := strings.TrimSpace(name)
	if clean == "" {
		return "", fmt.Errorf("no collection name given: name the collection in the node drawer")
	}
	if !collectionName.MatchString(clean) || strings.Contains(clean, "..") {
		return "", fmt.Errorf("%q is not a usable collection name: use letters, digits, dot, dash and underscore only, starting with a letter or digit (no path separators)", name)
	}
	return clean, nil
}

// manifestPrefix is the file the engine writes into a collection directory to
// describe its segments ("manifest.0", and higher numbers as it compacts). Its
// presence is what tells a collection apart from any other directory a user
// happens to keep alongside one.
const manifestPrefix = "manifest."

// IsCollectionDir reports whether a directory holds a zvec collection.
func IsCollectionDir(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), manifestPrefix) {
			return true
		}
	}
	return false
}

// ListCollections returns the names of every collection in the data directory,
// sorted. A data directory that does not exist yet holds no collections, which
// is not an error — it is what a new profile looks like before its first
// create.
func (c Config) ListCollections() ([]string, error) {
	entries, err := os.ReadDir(c.DataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("cannot read the data directory %s: %w", c.DataDir, err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := CheckName(entry.Name()); err != nil {
			continue
		}
		if IsCollectionDir(filepath.Join(c.DataDir, entry.Name())) {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

// CheckDir makes sure the data directory is usable, creating it when it does
// not exist yet. A read-only profile never creates anything — it is a reader,
// and silently making the directory it was pointed at would hide a typo.
func (c Config) CheckDir() error {
	info, err := os.Stat(c.DataDir)
	switch {
	case err == nil && !info.IsDir():
		return fmt.Errorf("%s is a file, not a directory: point the profile's data directory at a directory", c.DataDir)
	case err == nil:
		return nil
	case !os.IsNotExist(err):
		return fmt.Errorf("cannot use the data directory %s: %w", c.DataDir, err)
	case c.ReadOnly:
		return fmt.Errorf("the data directory %s does not exist, and this profile is read-only: create it, or untick read-only", c.DataDir)
	}
	if err := os.MkdirAll(c.DataDir, 0o750); err != nil {
		return fmt.Errorf("cannot create the data directory %s: %w", c.DataDir, err)
	}
	return nil
}
