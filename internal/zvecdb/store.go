package zvecdb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	zvec "github.com/zvec-ai/zvec-go"
)

// Store keeps every collection this process has touched open, keyed by its
// directory.
//
// This is not a cache for speed. An engine collection takes an exclusive lock
// on its directory (a LOCK file), so a second open of the same path is refused
// outright — "Can't lock read-write collection". Opening per job would
// therefore fail the moment two jobs on the same collection overlapped, and
// closing after each one would throw away the in-memory index. Holding one
// handle per directory and sharing it is the only correct shape.
//
// Because collections stay open, two plugin processes must not be pointed at
// one data directory: the second one's first write fails on the lock. Run one
// plugin per data directory, or give each its own.
type Store struct {
	mu     sync.Mutex
	open   map[string]*Handle
	closed bool
}

func NewStore() *Store { return &Store{open: map[string]*Handle{}} }

// Handle is one open collection, with the lock that orders access to it and
// the field types a document needs to be read or written.
//
// Reads take the read side, writes the write side. The engine has its own query
// thread pool and is built for concurrent search, so searches do not block each
// other; a write is serialised against them because a collection is a mutable
// object and nothing in the C API promises otherwise.
type Handle struct {
	// Name is the collection's name, which is also its directory's base name.
	Name string
	// Path is the collection's directory.
	Path string
	// Spec is the declared schema when this collection has a sidecar, and nil
	// otherwise — a collection created by another tool still works, it just
	// cannot volunteer which field holds its vectors.
	Spec *Spec

	mu         sync.RWMutex
	collection *zvec.Collection
	schema     *zvec.CollectionSchema

	typesMu sync.Mutex
	types   map[string]fieldType
}

// fieldType is what writing or reading one field needs to know.
type fieldType struct {
	dataType zvec.DataType
	dim      uint32
	vector   bool
	known    bool
}

// maxOpen bounds how many collections one process keeps open. A profile with
// more than this many collections in play evicts the least recently used one,
// which costs the next job on it a reopen rather than failing it.
const maxOpen = 32

// Open returns the handle for a collection, opening it on first use.
//
// A collection that does not exist is reported as such rather than created:
// creation declares a schema, and guessing one from a search would make a
// mistyped collection name look like an empty collection.
func (s *Store) Open(cfg Config, name string) (*Handle, error) {
	if err := InitLibrary(); err != nil {
		return nil, err
	}
	path, err := cfg.CollectionPath(name)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openLocked(cfg, name, path)
}

// openLocked is Open once the name has been checked and the pool lock is held,
// so Create can adopt an existing collection without releasing it.
func (s *Store) openLocked(cfg Config, name, path string) (*Handle, error) {
	if s.closed {
		return nil, fmt.Errorf("the plugin is shutting down")
	}
	if handle, ok := s.open[path]; ok {
		return handle, nil
	}

	if !IsCollectionDir(path) {
		if _, err := os.Stat(path); err == nil {
			return nil, fmt.Errorf("%s exists but is not a zvec collection: it holds no manifest file", path)
		}
		known, _ := cfg.ListCollections()
		if len(known) == 0 {
			return nil, fmt.Errorf("there is no collection %q in %s, and it holds none yet: create one with the Create collection action", name, cfg.DataDir)
		}
		return nil, fmt.Errorf("there is no collection %q in %s (it holds: %s)", name, cfg.DataDir, strings.Join(known, ", "))
	}

	options, err := cfg.options()
	if err != nil {
		return nil, err
	}
	if options != nil {
		defer options.Destroy()
	}
	collection, err := zvec.Open(path, options)
	if err != nil {
		return nil, openError(path, err)
	}
	handle, err := s.adopt(path, name, collection)
	if err != nil {
		_ = collection.Close()
		return nil, err
	}
	return handle, nil
}

// Create makes a collection from a declared spec and leaves it open, so the
// upsert that usually follows it reuses the same handle.
//
// When ifNotExists is set an existing collection is adopted instead of being
// an error, which is what makes a create action safe to leave in a flow that
// runs more than once.
func (s *Store) Create(cfg Config, name string, spec *Spec, ifNotExists bool) (*Handle, bool, error) {
	if err := InitLibrary(); err != nil {
		return nil, false, err
	}
	if cfg.ReadOnly {
		return nil, false, fmt.Errorf("this settings profile is read-only: untick read-only on the profile to create a collection in %s", cfg.DataDir)
	}
	if err := cfg.CheckDir(); err != nil {
		return nil, false, err
	}
	path, err := cfg.CollectionPath(name)
	if err != nil {
		return nil, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false, fmt.Errorf("the plugin is shutting down")
	}

	if handle, ok := s.open[path]; ok {
		if ifNotExists {
			return handle, false, nil
		}
		return nil, false, fmt.Errorf("the collection %q already exists in %s: tick \"only if missing\" to leave it alone, or drop it first", name, cfg.DataDir)
	}
	if IsCollectionDir(path) {
		if !ifNotExists {
			return nil, false, fmt.Errorf("the collection %q already exists in %s: tick \"only if missing\" to leave it alone, or drop it first", name, cfg.DataDir)
		}
		handle, err := s.openLocked(cfg, name, path)
		return handle, false, err
	}
	// A directory that exists but holds no manifest is not ours to write into:
	// the engine refuses to create over an existing path, and removing it to
	// make room would be a destructive guess.
	if _, err := os.Stat(path); err == nil {
		return nil, false, fmt.Errorf("%s already exists and is not a zvec collection: move it aside, or use another collection name", path)
	}

	schema, err := spec.Build(name)
	if err != nil {
		return nil, false, err
	}
	defer schema.Destroy()

	options, err := cfg.options()
	if err != nil {
		return nil, false, err
	}
	if options != nil {
		defer options.Destroy()
	}

	collection, err := zvec.CreateAndOpen(path, schema, options)
	if err != nil {
		return nil, false, fmt.Errorf("cannot create the collection %q in %s: %w", name, cfg.DataDir, err)
	}
	// The sidecar is written after the collection exists, and a failure to
	// write it is reported: a collection whose vector field cannot be recalled
	// is usable but awkward, and silently degrading would hide the cause.
	if err := writeSidecar(path, spec); err != nil {
		_ = collection.Close()
		return nil, false, err
	}
	handle, err := s.adopt(path, name, collection)
	if err != nil {
		_ = collection.Close()
		return nil, false, err
	}
	return handle, true, nil
}

// Drop closes and deletes a collection. It is irreversible, which is why the
// action behind it asks the author to confirm.
func (s *Store) Drop(cfg Config, name string) error {
	if err := InitLibrary(); err != nil {
		return err
	}
	if cfg.ReadOnly {
		return fmt.Errorf("this settings profile is read-only: it cannot drop %q", name)
	}
	path, err := cfg.CollectionPath(name)
	if err != nil {
		return err
	}

	s.mu.Lock()
	handle := s.open[path]
	delete(s.open, path)
	s.mu.Unlock()

	if handle != nil {
		// Take the write lock so a search in flight finishes before the
		// collection under it is destroyed, and never give it back.
		handle.mu.Lock()
		if err := handle.collection.Destroy(); err != nil {
			handle.mu.Unlock()
			return fmt.Errorf("cannot drop the collection %q: %w", name, err)
		}
		handle.release()
		handle.mu.Unlock()
	} else {
		if !IsCollectionDir(path) {
			return fmt.Errorf("there is no collection %q in %s", name, cfg.DataDir)
		}
		collection, err := zvec.Open(path, nil)
		if err != nil {
			return openError(path, err)
		}
		if err := collection.Destroy(); err != nil {
			return fmt.Errorf("cannot drop the collection %q: %w", name, err)
		}
	}

	_ = os.Remove(sidecarPath(path))
	// Destroy empties the directory; remove the directory itself so the
	// collection stops appearing in a listing of the data directory.
	_ = os.Remove(path)
	return nil
}

// Close closes every open collection. Call it before shutting the library down.
func (s *Store) Close() {
	s.mu.Lock()
	handles := make([]*Handle, 0, len(s.open))
	for path, handle := range s.open {
		handles = append(handles, handle)
		delete(s.open, path)
	}
	s.closed = true
	s.mu.Unlock()

	for _, handle := range handles {
		handle.mu.Lock()
		if handle.collection != nil {
			_ = handle.collection.Flush()
			_ = handle.collection.Close()
		}
		handle.release()
		handle.mu.Unlock()
	}
}

// adopt files a freshly opened collection in the pool. Called with s.mu held.
func (s *Store) adopt(path, name string, collection *zvec.Collection) (*Handle, error) {
	schema, err := collection.GetSchema()
	if err != nil {
		return nil, fmt.Errorf("cannot read the schema of %q: %w", name, err)
	}

	handle := &Handle{
		Name:       name,
		Path:       path,
		Spec:       readSidecar(path),
		collection: collection,
		schema:     schema,
		types:      map[string]fieldType{},
	}
	s.evictIfFull()
	s.open[path] = handle
	return handle, nil
}

// evictIfFull closes one collection when the pool is full. Called with s.mu
// held. "Least recently used" would need bookkeeping on every read for a cap a
// deployment is unlikely to reach, so one arbitrary idle handle is dropped
// instead — and a handle that is busy is left alone, because closing a
// collection out from under a running search is worse than holding one more.
func (s *Store) evictIfFull() {
	if len(s.open) < maxOpen {
		return
	}
	for path, handle := range s.open {
		if !handle.mu.TryLock() {
			continue
		}
		if handle.collection != nil {
			_ = handle.collection.Flush()
			_ = handle.collection.Close()
		}
		handle.release()
		handle.mu.Unlock()
		delete(s.open, path)
		return
	}
}

// release drops the engine handles a collection held. Called with the write
// lock held.
func (h *Handle) release() {
	if h.schema != nil {
		h.schema.Destroy()
		h.schema = nil
	}
	h.collection = nil
}

// options renders the collection options a profile asks for, or nil when it
// asks for the engine's defaults.
func (c Config) options() (*zvec.CollectionOptions, error) {
	if !c.ReadOnly && !c.EnableMmap {
		return nil, nil
	}
	options := zvec.NewCollectionOptions()
	if options == nil {
		return nil, fmt.Errorf("%w: cannot allocate collection options", ErrLibrary)
	}
	if c.ReadOnly {
		if err := options.SetReadOnly(true); err != nil {
			options.Destroy()
			return nil, fmt.Errorf("cannot open read-only: %w", err)
		}
	}
	if c.EnableMmap {
		if err := options.SetEnableMmap(true); err != nil {
			options.Destroy()
			return nil, fmt.Errorf("cannot enable mmap: %w", err)
		}
	}
	return options, nil
}

// openError explains the failure a locked collection produces, which is the one
// an operator is most likely to hit and the least self-explanatory.
func openError(path string, err error) error {
	if strings.Contains(err.Error(), "lock") {
		return fmt.Errorf("cannot open %s: another process holds it open (%w). A zvec collection takes an exclusive lock: run one plugin per data directory, and check for a stale LOCK file left by a crash", path, err)
	}
	return fmt.Errorf("cannot open %s: %w", path, err)
}

// ---------------------------------------------------------------- sidecar --

// sidecarName is where a collection's declared spec is kept: beside the
// collection directory, not inside it, so the engine's own files are never
// mixed with ours.
const sidecarName = ".inflow-schema.json"

func sidecarPath(collectionPath string) string {
	return filepath.Join(filepath.Dir(collectionPath), filepath.Base(collectionPath)+sidecarName)
}

func writeSidecar(collectionPath string, spec *Spec) error {
	encoded, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot record the collection's schema: %w", err)
	}
	if err := os.WriteFile(sidecarPath(collectionPath), append(encoded, '\n'), 0o640); err != nil {
		return fmt.Errorf("cannot write %s: %w", sidecarPath(collectionPath), err)
	}
	return nil
}

// readSidecar reads a collection's declared spec, answering nil when there is
// none to read. A collection made by another tool, or one whose sidecar was
// lost, is still perfectly usable — every action that needs a field name takes
// one, and only falls back to the spec to fill the default in.
func readSidecar(collectionPath string) *Spec {
	raw, err := os.ReadFile(sidecarPath(collectionPath))
	if err != nil {
		return nil
	}
	var spec Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil
	}
	return &spec
}
