//go:build integration

// These tests run against the real zvec engine, so they need its shared
// library. Run them with:
//
//	ZVEC_LIBRARY_PATH=/path/to/libzvec_c_api.so go test -tags integration ./...
//
// Everything else in this package is tested without it.
package zvecdb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// store opens a store over a temporary data directory, with the engine running.
func store(t *testing.T) (*Store, Config) {
	t.Helper()
	if err := InitLibrary(); err != nil {
		t.Skipf("the zvec engine is not available: %v", err)
	}

	cfg := Config{DataDir: t.TempDir()}
	s := NewStore()
	t.Cleanup(s.Close)
	return s, cfg
}

// sampleSpec is a collection with both halves: vectors to search by meaning,
// text to search by word, and two scalars to filter on.
func sampleSpec() *Spec {
	return &Spec{
		Vector: &VectorSpec{Field: "embedding", Dim: 4, Metric: "cosine", Index: "hnsw"},
		Text:   &TextSpec{Field: "content", Tokenizer: "standard", Filters: []string{"lowercase"}},
		Fields: []FieldSpec{
			{Name: "category", Type: "string", Filterable: true},
			{Name: "qty", Type: "int64", Filterable: true},
		},
	}
}

func sampleDocs() []Document {
	return []Document{
		{Key: "d1", Text: "The quick brown fox jumps over the lazy dog", Vector: []float32{1, 0, 0, 0}, Fields: map[string]any{"category": "animals", "qty": 3}},
		{Key: "d2", Text: "A fast red fox runs through the forest", Vector: []float32{0.9, 0.1, 0, 0}, Fields: map[string]any{"category": "animals", "qty": 5}},
		{Key: "d3", Text: "Dogs and cats are popular household pets", Vector: []float32{0, 1, 0, 0}, Fields: map[string]any{"category": "pets", "qty": 7}},
		{Key: "d4", Text: "A waterproof rain jacket for hiking", Vector: []float32{0, 0, 1, 0}, Fields: map[string]any{"category": "gear", "qty": 2}},
	}
}

func seeded(t *testing.T) (*Store, Config, *Handle) {
	t.Helper()
	s, cfg := store(t)
	handle, created, err := s.Create(cfg, "docs", sampleSpec(), true)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !created {
		t.Fatal("Create reported an existing collection in a fresh directory")
	}
	if _, err := handle.Write(WriteUpsert, sampleDocs(), "embedding", "content", true); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return s, cfg, handle
}

func keysOf(docs []Document) []string {
	keys := make([]string, 0, len(docs))
	for _, doc := range docs {
		keys = append(keys, doc.Key)
	}
	return keys
}

func TestCreateWritesASidecarAndOpensTheCollection(t *testing.T) {
	s, cfg, handle := seeded(t)

	if handle.Spec.VectorField() != "embedding" || handle.Spec.TextField() != "content" {
		t.Fatalf("spec = %+v, want it to remember both fields", handle.Spec)
	}
	// The sidecar lives beside the collection, never inside it — the engine's
	// own directory stays untouched.
	entries, err := os.ReadDir(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	var sawSidecar bool
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), sidecarName) {
			sawSidecar = true
		}
	}
	if !sawSidecar {
		t.Fatal("no sidecar was written beside the collection")
	}

	// A fresh store reads the spec back off disk, which is the whole point of
	// writing it: the vector field is remembered across restarts.
	fresh := NewStore()
	defer fresh.Close()
	s.Close()
	reopened, err := fresh.Open(cfg, "docs")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Spec.VectorField() != "embedding" {
		t.Fatalf("spec after reopen = %+v", reopened.Spec)
	}
}

// The engine refuses a second handle on a collection, so the pool has to hand
// back the one it already holds rather than opening another.
func TestOpenSharesOneHandlePerCollection(t *testing.T) {
	s, cfg, handle := seeded(t)
	again, err := s.Open(cfg, "docs")
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	if again != handle {
		t.Fatal("Open returned a second handle for one collection; the engine's lock would refuse it")
	}
}

func TestCreateIsIdempotentWhenAskedToBe(t *testing.T) {
	s, cfg, _ := seeded(t)

	_, created, err := s.Create(cfg, "docs", sampleSpec(), true)
	if err != nil {
		t.Fatalf("second Create with ifNotExists: %v", err)
	}
	if created {
		t.Fatal("the second Create reported that it created the collection")
	}
	if _, _, err := s.Create(cfg, "docs", sampleSpec(), false); err == nil {
		t.Fatal("creating an existing collection without ifNotExists was allowed")
	}
}

func TestOpenNamesTheCollectionsThatDoExist(t *testing.T) {
	s, cfg, _ := seeded(t)
	_, err := s.Open(cfg, "nope")
	if err == nil {
		t.Fatal("opening a missing collection succeeded")
	}
	if !strings.Contains(err.Error(), "docs") {
		t.Fatalf("err = %q, want it to list the collections that do exist", err)
	}
}

func TestVectorSearchHonoursItsFilter(t *testing.T) {
	_, _, handle := seeded(t)

	docs, err := handle.Query(Search{Field: "embedding", Vector: []float32{1, 0, 0, 0}, TopK: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(docs) != 4 {
		t.Fatalf("unfiltered search returned %d documents, want 4", len(docs))
	}
	if docs[0].Key != "d1" {
		t.Fatalf("nearest = %q, want d1", docs[0].Key)
	}
	// The collection's own fields are hoisted, so a result reads the way an
	// upsert is written.
	if docs[0].Text == "" {
		t.Fatalf("result carries no text: %+v", docs[0])
	}

	filtered, err := handle.Query(Search{
		Field:  "embedding",
		Vector: []float32{1, 0, 0, 0},
		TopK:   10,
		Filter: "category = 'animals' AND qty > 3",
	})
	if err != nil {
		t.Fatalf("filtered Query: %v", err)
	}
	if got := keysOf(filtered); len(got) != 1 || got[0] != "d2" {
		t.Fatalf("filtered search returned %v, want [d2]", got)
	}
}

// The filter language is SQL-like, and == is the mistake everyone makes once.
// The error has to say so rather than hand back a parser token dump.
func TestAFilterWrittenWithDoubleEqualsSaysWhatIsWrong(t *testing.T) {
	_, _, handle := seeded(t)
	_, err := handle.Query(Search{Field: "embedding", Vector: []float32{1, 0, 0, 0}, Filter: "category == 'animals'"})
	if err == nil {
		t.Fatal("a == filter was accepted")
	}
	if !strings.Contains(err.Error(), "single =") {
		t.Fatalf("err = %q, want it to point at the single =", err)
	}
}

func TestAQuoteInsideAFilterValueIsEscaped(t *testing.T) {
	_, _, handle := seeded(t)
	if _, err := handle.Write(WriteUpsert, []Document{{
		Key: "d5", Text: "an irish pub", Vector: []float32{0, 0, 0, 1},
		Fields: map[string]any{"category": "O'Brien", "qty": 1},
	}}, "embedding", "content", true); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// This is the expression the actions layer builds for a value with an
	// apostrophe in it: the engine escapes with a backslash, not by doubling.
	docs, err := handle.Query(Search{Field: "embedding", Vector: []float32{0, 0, 0, 1}, TopK: 10, Filter: `category = 'O\'Brien'`})
	if err != nil {
		t.Fatalf("Query with an escaped quote: %v", err)
	}
	if got := keysOf(docs); len(got) != 1 || got[0] != "d5" {
		t.Fatalf("search returned %v, want [d5]", got)
	}
}

func TestFullTextSearchMatchesWordsAndExpressions(t *testing.T) {
	_, _, handle := seeded(t)

	docs, err := handle.QueryFTS(FTS{Field: "content", Match: "fox", TopK: 10})
	if err != nil {
		t.Fatalf("QueryFTS: %v", err)
	}
	if got := keysOf(docs); len(got) != 2 {
		t.Fatalf("match 'fox' returned %v, want the two fox documents", got)
	}

	expression, err := handle.QueryFTS(FTS{Field: "content", Query: "fox AND forest", TopK: 10})
	if err != nil {
		t.Fatalf("QueryFTS with an expression: %v", err)
	}
	if got := keysOf(expression); len(got) != 1 || got[0] != "d2" {
		t.Fatalf("'fox AND forest' returned %v, want [d2]", got)
	}
}

// Searching a field with no full-text index silently returns nothing, which
// looks like missing data. It has to be refused with the reason instead.
func TestFullTextSearchRefusesAFieldWithNoIndex(t *testing.T) {
	_, _, handle := seeded(t)
	_, err := handle.QueryFTS(FTS{Field: "category", Match: "animals"})
	if err == nil {
		t.Fatal("a full-text search on an unindexed field was accepted")
	}
	if !strings.Contains(err.Error(), "no full-text index") {
		t.Fatalf("err = %q", err)
	}
}

func TestHybridSearchFusesBothHalves(t *testing.T) {
	_, _, handle := seeded(t)

	// The vector half points away from the dog documents; the text half asks
	// for them. A fused ranking has to carry both in.
	docs, err := handle.QueryHybrid(Hybrid{
		VectorField: "embedding",
		Vector:      []float32{0, 0, 1, 0}, // nearest d4
		TextField:   "content",
		Match:       "dog",
		TopK:        3,
		Rerank:      "rrf",
	})
	if err != nil {
		t.Fatalf("QueryHybrid: %v", err)
	}
	got := keysOf(docs)
	if len(got) == 0 {
		t.Fatal("hybrid search returned nothing")
	}
	found := map[string]bool{}
	for _, key := range got {
		found[key] = true
	}
	if !found["d4"] {
		t.Fatalf("hybrid results %v do not include d4, which only the vector half finds", got)
	}
	if !found["d1"] && !found["d3"] {
		t.Fatalf("hybrid results %v include neither dog document, which only the text half finds", got)
	}
}

func TestHybridSearchAcceptsWeightedReranking(t *testing.T) {
	_, _, handle := seeded(t)
	docs, err := handle.QueryHybrid(Hybrid{
		VectorField:  "embedding",
		Vector:       []float32{1, 0, 0, 0},
		TextField:    "content",
		Match:        "fox",
		TopK:         3,
		Rerank:       "weighted",
		VectorWeight: 0.7,
		TextWeight:   0.3,
	})
	if err != nil {
		t.Fatalf("weighted QueryHybrid: %v", err)
	}
	if len(docs) == 0 {
		t.Fatal("weighted hybrid search returned nothing")
	}
}

// Weights address the halves by position, so asking for them with only one
// half is a mistake worth naming rather than quietly reranking nothing. (RRF
// with one half is fine, and falls back to that half's own search.)
func TestWeightedRerankingNeedsBothHalves(t *testing.T) {
	_, _, handle := seeded(t)
	_, err := handle.QueryHybrid(Hybrid{
		TextField: "content",
		Match:     "fox",
		Rerank:    "weighted",
	})
	if err == nil {
		t.Fatal("weighted reranking with one half was accepted")
	}
	if !strings.Contains(err.Error(), "both halves") {
		t.Fatalf("err = %q", err)
	}
}

// A hybrid node pointed at a collection with only one searchable half should
// still work, as that half's search.
func TestHybridSearchRunsWithOnlyOneHalf(t *testing.T) {
	_, _, handle := seeded(t)
	docs, err := handle.QueryHybrid(Hybrid{TextField: "content", Match: "fox", TopK: 5})
	if err != nil {
		t.Fatalf("text-only hybrid: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("text-only hybrid returned %v, want the two fox documents", keysOf(docs))
	}
}

func TestSearchParamsAreAppliedToWhicheverIndexTheFieldHas(t *testing.T) {
	_, _, handle := seeded(t)
	if _, err := handle.Query(Search{
		Field:  "embedding",
		Vector: []float32{1, 0, 0, 0},
		TopK:   2,
		Params: map[string]any{"ef": 64},
	}); err != nil {
		t.Fatalf("Query with HNSW search params: %v", err)
	}
	// A knob spelt the other way round still lands.
	if _, err := handle.Query(Search{
		Field:  "embedding",
		Vector: []float32{1, 0, 0, 0},
		Params: map[string]any{"is_using_refiner": true},
	}); err != nil {
		t.Fatalf("Query with a snake_case knob: %v", err)
	}
	// A knob that is plainly a mistake is refused rather than ignored.
	if _, err := handle.Query(Search{
		Field:  "embedding",
		Vector: []float32{1, 0, 0, 0},
		Params: map[string]any{"ef": "lots"},
	}); err == nil {
		t.Fatal("a non-numeric ef was accepted")
	}
}

func TestWriteRefusesAVectorOfTheWrongLength(t *testing.T) {
	_, _, handle := seeded(t)
	_, err := handle.Write(WriteUpsert, []Document{{Key: "bad", Vector: []float32{1, 2}}}, "embedding", "content", true)
	if err == nil {
		t.Fatal("a 2-dimensional vector was written into a 4-dimensional field")
	}
	if !strings.Contains(err.Error(), "dimension") {
		t.Fatalf("err = %q", err)
	}
}

func TestWriteRefusesAFieldTheCollectionNeverDeclared(t *testing.T) {
	_, _, handle := seeded(t)
	_, err := handle.Write(WriteUpsert, []Document{{
		Key: "x", Vector: []float32{1, 0, 0, 0}, Fields: map[string]any{"nope": "value"},
	}}, "embedding", "content", true)
	if err == nil {
		t.Fatal("an undeclared field was written")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %q, want it to name the field", err)
	}
}

// A document that fails validation must not leave the rest of its batch
// half-written.
func TestABadDocumentFailsTheWholeBatch(t *testing.T) {
	_, _, handle := seeded(t)
	before, err := handle.Stats()
	if err != nil {
		t.Fatal(err)
	}

	_, err = handle.Write(WriteUpsert, []Document{
		{Key: "ok1", Vector: []float32{1, 0, 0, 0}},
		{Key: "bad", Vector: []float32{1, 0}},
	}, "embedding", "content", true)
	if err == nil {
		t.Fatal("a batch with a bad document was written")
	}

	after, err := handle.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if after.Documents != before.Documents {
		t.Fatalf("document count went from %d to %d; the good half of the batch was written", before.Documents, after.Documents)
	}
}

func TestUpdateChangesOnlyTheFieldsItCarries(t *testing.T) {
	_, _, handle := seeded(t)
	if _, err := handle.Write(WriteUpdate, []Document{{
		Key: "d1", Fields: map[string]any{"qty": 99},
	}}, "embedding", "content", true); err != nil {
		t.Fatalf("Write(update): %v", err)
	}

	docs, err := handle.Fetch([]string{"d1"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("fetched %d documents", len(docs))
	}
	if docs[0].Fields["qty"] != int64(99) {
		t.Fatalf("qty = %#v, want 99", docs[0].Fields["qty"])
	}
	if docs[0].Text == "" {
		t.Fatal("the update dropped the document's text; it should have left it alone")
	}
}

func TestFetchReportsOnlyWhatItFound(t *testing.T) {
	_, _, handle := seeded(t)
	docs, err := handle.Fetch([]string{"d1", "missing", "d3"}, []string{"category"}, false)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("Fetch returned %v, want the two that exist", keysOf(docs))
	}
	for _, doc := range docs {
		if _, asked := doc.Fields["category"]; !asked {
			t.Fatalf("document %q has no category: %+v", doc.Key, doc.Fields)
		}
	}
}

func TestScanPagesThroughTheCollection(t *testing.T) {
	_, _, handle := seeded(t)

	first, more, err := handle.Scan(2, 0, nil, false)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(first) != 2 || !more {
		t.Fatalf("first page = %v (more=%v), want 2 and more", keysOf(first), more)
	}

	second, more, err := handle.Scan(2, 2, nil, false)
	if err != nil {
		t.Fatalf("Scan page 2: %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("second page = %v, want 2", keysOf(second))
	}
	if more {
		t.Fatal("the second page reports more, but the collection holds four documents")
	}

	seen := map[string]bool{}
	for _, doc := range append(first, second...) {
		if seen[doc.Key] {
			t.Fatalf("document %q appeared on both pages", doc.Key)
		}
		seen[doc.Key] = true
	}
	if len(seen) != 4 {
		t.Fatalf("paging saw %d documents, want 4", len(seen))
	}
}

func TestStatsDescribeTheCollection(t *testing.T) {
	_, _, handle := seeded(t)
	stats, err := handle.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Documents != 4 {
		t.Fatalf("documents = %d, want 4", stats.Documents)
	}

	byName := map[string]FieldDescription{}
	for _, field := range stats.Fields {
		byName[field.Name] = field
	}
	for _, want := range []string{"embedding", "content", "category", "qty"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("fields = %+v, want %q among them", stats.Fields, want)
		}
	}
	if !byName["embedding"].Vector || byName["embedding"].Dim != 4 {
		t.Fatalf("embedding described as %+v", byName["embedding"])
	}
	if byName["content"].Index != "FTS" {
		t.Fatalf("content described as %+v, want an FTS index", byName["content"])
	}
}

func TestDeleteByKeyAndByFilter(t *testing.T) {
	_, _, handle := seeded(t)

	result, err := handle.Delete([]string{"d4"}, true)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if result.Written != 1 {
		t.Fatalf("deleted %d, want 1", result.Written)
	}
	if err := handle.DeleteByFilter("category = 'pets'", true); err != nil {
		t.Fatalf("DeleteByFilter: %v", err)
	}

	stats, err := handle.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Documents != 2 {
		t.Fatalf("documents = %d, want 2", stats.Documents)
	}
}

func TestMaintainFlushesAndOptimizes(t *testing.T) {
	_, _, handle := seeded(t)
	if err := handle.Maintain(true, true); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	if _, err := handle.Query(Search{Field: "embedding", Vector: []float32{1, 0, 0, 0}, TopK: 1}); err != nil {
		t.Fatalf("Query after optimize: %v", err)
	}
}

func TestDropRemovesTheCollectionAndItsSidecar(t *testing.T) {
	s, cfg, _ := seeded(t)

	if err := s.Drop(cfg, "docs"); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	names, err := cfg.ListCollections()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Fatalf("collections after drop = %v", names)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "docs"+sidecarName)); !os.IsNotExist(err) {
		t.Fatal("the sidecar outlived the collection")
	}
	// The name is free again afterwards.
	if _, _, err := s.Create(cfg, "docs", sampleSpec(), false); err != nil {
		t.Fatalf("recreate after drop: %v", err)
	}
}

func TestAReadOnlyProfileCannotWrite(t *testing.T) {
	s, cfg := store(t)
	if _, _, err := s.Create(cfg, "docs", sampleSpec(), true); err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.Close()

	readers := NewStore()
	defer readers.Close()
	readOnly := cfg
	readOnly.ReadOnly = true

	handle, err := readers.Open(readOnly, "docs")
	if err != nil {
		t.Fatalf("Open read-only: %v", err)
	}
	if _, err := handle.Write(WriteUpsert, sampleDocs(), "embedding", "content", true); err == nil {
		t.Fatal("a read-only profile wrote to the collection")
	}
	if _, _, err := readers.Create(readOnly, "another", sampleSpec(), true); err == nil {
		t.Fatal("a read-only profile created a collection")
	}
}

// A text-only collection is a legitimate shape: no vectors at all.
func TestATextOnlyCollectionWorks(t *testing.T) {
	s, cfg := store(t)
	spec := &Spec{Text: &TextSpec{Field: "content", Tokenizer: "standard", Filters: []string{"lowercase"}}}

	handle, _, err := s.Create(cfg, "notes", spec, false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := handle.Write(WriteUpsert, []Document{
		{Key: "n1", Text: "a note about foxes"},
		{Key: "n2", Text: "a note about jackets"},
	}, "", "content", true); err != nil {
		t.Fatalf("Write: %v", err)
	}

	docs, err := handle.QueryFTS(FTS{Field: "content", Match: "foxes", TopK: 5})
	if err != nil {
		t.Fatalf("QueryFTS: %v", err)
	}
	if got := keysOf(docs); len(got) != 1 || got[0] != "n1" {
		t.Fatalf("search returned %v, want [n1]", got)
	}
}

func TestStoreRefusesANameThatEscapesTheDataDirectory(t *testing.T) {
	s, cfg := store(t)
	for _, name := range []string{"../escape", "/etc", "a/b"} {
		if _, err := s.Open(cfg, name); err == nil {
			t.Fatalf("Open(%q) was allowed", name)
		}
		if _, _, err := s.Create(cfg, name, sampleSpec(), true); err == nil {
			t.Fatalf("Create(%q) was allowed", name)
		}
	}
}
