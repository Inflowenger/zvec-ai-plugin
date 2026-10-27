package zvecdb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ErrNoSettings is returned when a call arrives without a usable data
// directory. It is phrased for the person looking at the node, because that is
// where the fix is.
var ErrNoSettings = errors.New("this node has no zvec storage: pick a settings profile in the node drawer (it needs a data directory) or create one from the plugin's set-up form")

// ErrLibrary is returned when the zvec native library could not be loaded or
// initialised. It names the environment variable that fixes it, since nothing
// about a flow can.
var ErrLibrary = errors.New("the zvec engine is not available on this host: install the zvec C-API shared library and point ZVEC_LIBRARY_PATH at it (see the plugin's README)")

// Config is a resolved zvec storage location: the directory holding the
// collections, plus how they should be opened.
type Config struct {
	// DataDir is the absolute path of the directory collections live in. Each
	// collection is a subdirectory named after it.
	DataDir string
	// ReadOnly opens every collection read-only, so the whole profile is a
	// reader. The engine refuses writes outright, which makes this a real
	// guarantee rather than a convention.
	ReadOnly bool
	// EnableMmap maps segment files instead of reading them into the process,
	// which trades page cache for resident memory on a large collection.
	EnableMmap bool
}

// ParseConfig reads the storage location out of the settings profile the
// platform ships with every action call as body.settings.
//
// The profile is a free-form key/value record — filled from this plugin's own
// set-up form when the platform could read it, and hand-typed otherwise. Keys
// are therefore matched leniently: case, spaces, dashes and underscores are
// ignored, and the usual synonyms are accepted.
func ParseConfig(settings map[string]any) (Config, error) {
	if len(settings) == 0 {
		return Config{}, ErrNoSettings
	}
	values := canonicalValues(settings)

	dir := pick(values, dataDirKeys)
	if dir == "" {
		// Something was configured, but none of it names a directory — most
		// likely another plugin's profile is bound to this node. Listing the
		// keys it did carry turns a support ticket into a five-second fix.
		return Config{}, fmt.Errorf("%w (the selected profile has none of these: %s)", ErrNoSettings, strings.Join(sortedKeys(settings), ", "))
	}

	dir, err := absDir(dir)
	if err != nil {
		return Config{}, err
	}
	return Config{
		DataDir:    dir,
		ReadOnly:   isTrue(pick(values, readOnlyKeys)),
		EnableMmap: isTrue(pick(values, mmapKeys)),
	}, nil
}

// absDir expands a leading ~ and makes the path absolute, so two profiles that
// name the same directory differently still share one open collection — the
// engine's lock is per directory, and a second handle on it would be refused.
func absDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand %q: no home directory for this process: %w", dir, err)
		}
		dir = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(dir, "~"), "/"))
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("%q is not a usable directory path: %w", dir, err)
	}
	return filepath.Clean(abs), nil
}

// Describe names this storage for a progress frame or an error, short enough to
// read on a node.
func (c Config) Describe() string {
	if c.ReadOnly {
		return c.DataDir + " (read-only)"
	}
	return c.DataDir
}

// EmbeddingSettings is the optional embedding provider carried by the same
// profile, as the keys this plugin's settings form writes. The embed package
// owns their meaning; this only lifts them out of the profile.
type EmbeddingSettings struct {
	Provider string
	Model    string
	APIKey   string
	BaseURL  string
}

// ParseEmbedding reads the embedding provider out of a settings profile. An
// absent or "none" provider is not an error: the actions then require a vector
// to be supplied directly, and only complain when text arrives with nowhere to
// embed it.
func ParseEmbedding(settings map[string]any) EmbeddingSettings {
	values := canonicalValues(settings)
	provider := strings.ToLower(pick(values, providerKeys))
	if provider == "none" {
		provider = ""
	}
	return EmbeddingSettings{
		Provider: provider,
		Model:    pick(values, modelKeys),
		APIKey:   pick(values, embedKeyKeys),
		BaseURL:  pick(values, embedBaseKeys),
	}
}

// Field synonyms, in preference order. Each entry is already canonical (lower
// case, letters and digits only) — see canonicalKey.
var (
	dataDirKeys   = []string{"datadir", "dir", "directory", "path", "datapath", "storage", "storagedir", "root", "rootdir", "basedir", "home"}
	readOnlyKeys  = []string{"readonly", "ro", "readonlymode"}
	mmapKeys      = []string{"enablemmap", "mmap", "usemmap", "memorymap"}
	providerKeys  = []string{"embeddingprovider", "embedprovider", "provider"}
	modelKeys     = []string{"embeddingmodel", "embedmodel", "model"}
	embedKeyKeys  = []string{"embeddingapikey", "embedapikey", "embeddingkey", "embedkey"}
	embedBaseKeys = []string{"embeddingbaseurl", "embedbaseurl", "embeddingurl", "embedurl"}
)

// canonicalValues flattens a profile into canonical-key → text, dropping the
// blanks so pick() falls through to the next synonym instead of stopping on an
// empty field.
func canonicalValues(settings map[string]any) map[string]string {
	values := make(map[string]string, len(settings))
	for key, value := range settings {
		if text := toString(value); text != "" {
			values[canonicalKey(key)] = text
		}
	}
	return values
}

func pick(values map[string]string, keys []string) string {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value
		}
	}
	return ""
}

// canonicalKey reduces a settings key to lower-case letters and digits, so
// "Data Dir", "data_dir" and "dataDir" are one key.
func canonicalKey(key string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// toString renders a profile value. The settings editor keeps whatever type a
// field held, so a number arrives as a float64 and a checkbox as a bool.
func toString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	case float64:
		if typed == float64(int64(typed)) {
			return fmt.Sprintf("%d", int64(typed))
		}
		return fmt.Sprintf("%v", typed)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", typed))
	}
}

// isTrue reads a checkbox out of a profile that may store it as a bool, a
// string, or a number.
func isTrue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "yes", "1", "t", "on":
		return true
	}
	return false
}

func sortedKeys(settings map[string]any) []string {
	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
