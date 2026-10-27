package zvecdb

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseConfigMatchesKeysLeniently(t *testing.T) {
	// A profile is often typed by hand, so these are all the same field.
	for _, key := range []string{"dataDir", "data_dir", "Data Dir", "DATADIR", "path", "directory", "storage"} {
		t.Run(key, func(t *testing.T) {
			cfg, err := ParseConfig(map[string]any{key: "/srv/vectors"})
			if err != nil {
				t.Fatalf("ParseConfig(%q) failed: %v", key, err)
			}
			if cfg.DataDir != "/srv/vectors" {
				t.Fatalf("DataDir = %q, want /srv/vectors", cfg.DataDir)
			}
		})
	}
}

func TestParseConfigReadsFlagsInWhateverTypeTheyWereStored(t *testing.T) {
	cfg, err := ParseConfig(map[string]any{
		"dataDir":    "/srv/vectors",
		"readOnly":   "yes",
		"enableMmap": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ReadOnly || !cfg.EnableMmap {
		t.Fatalf("flags not read: %+v", cfg)
	}
}

func TestParseConfigMakesThePathAbsolute(t *testing.T) {
	// Two spellings of one directory must resolve to one path, because the
	// engine's lock is per directory and a second handle on it is refused.
	cfg, err := ParseConfig(map[string]any{"dataDir": "./data/../data"})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.DataDir) {
		t.Fatalf("DataDir = %q, want an absolute path", cfg.DataDir)
	}
	if strings.Contains(cfg.DataDir, "..") {
		t.Fatalf("DataDir = %q, want it cleaned", cfg.DataDir)
	}
}

func TestParseConfigRejectsAnEmptyProfile(t *testing.T) {
	if _, err := ParseConfig(nil); !errors.Is(err, ErrNoSettings) {
		t.Fatalf("err = %v, want ErrNoSettings", err)
	}
}

// A profile belonging to another plugin is the likeliest wrong binding, so the
// message lists what the profile did carry.
func TestParseConfigNamesTheKeysAWrongProfileCarried(t *testing.T) {
	_, err := ParseConfig(map[string]any{"baseUrl": "https://acme.example", "apiToken": "secret"})
	if !errors.Is(err, ErrNoSettings) {
		t.Fatalf("err = %v, want ErrNoSettings", err)
	}
	if !strings.Contains(err.Error(), "apiToken") || !strings.Contains(err.Error(), "baseUrl") {
		t.Fatalf("err = %q, want it to list the keys the profile had", err)
	}
}

func TestParseEmbeddingTreatsNoneAsUnset(t *testing.T) {
	for _, value := range []string{"", "none", "None"} {
		got := ParseEmbedding(map[string]any{"dataDir": "/d", "embeddingProvider": value})
		if got.Provider != "" {
			t.Fatalf("provider %q read as %q, want empty", value, got.Provider)
		}
	}
	got := ParseEmbedding(map[string]any{
		"embedding_provider": "OpenAI",
		"embedding model":    "text-embedding-3-small",
		"embeddingApiKey":    "sk-test",
		"embedding base url": "https://proxy.example",
	})
	if got.Provider != "openai" || got.Model != "text-embedding-3-small" || got.APIKey != "sk-test" || got.BaseURL != "https://proxy.example" {
		t.Fatalf("embedding settings not read leniently: %+v", got)
	}
}

// A collection name becomes a path under the data directory, so it is the one
// place a flow's input could otherwise reach outside the profile's storage.
func TestCheckNameRefusesAnythingThatEscapesTheDataDirectory(t *testing.T) {
	for _, name := range []string{
		"..",
		"../etc",
		"../../etc/passwd",
		"a/b",
		`a\b`,
		"/absolute",
		".hidden",
		"",
		"   ",
		"with space",
		"semi;colon",
		"sub/../../../root",
		strings.Repeat("x", 129),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := CheckName(name); err == nil {
				t.Fatalf("CheckName(%q) was accepted", name)
			}
		})
	}
}

func TestCheckNameAcceptsOrdinaryNames(t *testing.T) {
	for _, name := range []string{"docs", "my-docs", "my_docs.v2", "a", "0"} {
		if _, err := CheckName(name); err != nil {
			t.Fatalf("CheckName(%q) was refused: %v", name, err)
		}
	}
}

func TestCollectionPathStaysUnderTheDataDirectory(t *testing.T) {
	cfg := Config{DataDir: "/srv/vectors"}
	path, err := cfg.CollectionPath(" docs ")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/srv/vectors/docs" {
		t.Fatalf("path = %q, want /srv/vectors/docs", path)
	}
	if _, err := cfg.CollectionPath("../escape"); err == nil {
		t.Fatal("a traversing name produced a path")
	}
}
