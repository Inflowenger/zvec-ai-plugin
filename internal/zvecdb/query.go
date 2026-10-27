package zvecdb

import (
	"fmt"
	"io"
	"strings"

	zvec "github.com/zvec-ai/zvec-go"
)

// WriteMode is how an upsert reaches the collection.
type WriteMode string

const (
	// WriteUpsert inserts a document, replacing one that already has its key.
	WriteUpsert WriteMode = "upsert"
	// WriteInsert only inserts; a key that already exists is an error.
	WriteInsert WriteMode = "insert"
	// WriteUpdate only updates, and only the fields the document carries —
	// which is how one field of a document is changed without resending its
	// vector.
	WriteUpdate WriteMode = "update"
)

// WriteResult is what a write reports back to the flow.
type WriteResult struct {
	Written uint64 `json:"written"`
	Failed  uint64 `json:"failed"`
}

// Write stores documents. vectorField and textField name where each document's
// Vector and Text belong.
func (h *Handle) Write(mode WriteMode, docs []Document, vectorField, textField string, flush bool) (WriteResult, error) {
	if len(docs) == 0 {
		return WriteResult{}, fmt.Errorf("no documents to write")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.collection == nil {
		return WriteResult{}, errClosed(h.Name)
	}

	// Documents are built before any of them is written, so a mistyped field on
	// the tenth document fails the action without having half-written the other
	// nine. The engine's own write is a single call, so this keeps the whole
	// action all-or-nothing in the cases a flow can cause.
	built := make([]*zvec.Doc, 0, len(docs))
	defer func() {
		for _, doc := range built {
			doc.Destroy()
		}
	}()
	for _, in := range docs {
		doc, err := h.toDoc(in, vectorField, textField)
		if err != nil {
			return WriteResult{}, err
		}
		built = append(built, doc)
	}

	var (
		result *zvec.WriteResult
		err    error
	)
	switch mode {
	case WriteInsert:
		result, err = h.collection.Insert(built)
	case WriteUpdate:
		result, err = h.collection.Update(built)
	case WriteUpsert, "":
		result, err = h.collection.Upsert(built)
	default:
		return WriteResult{}, fmt.Errorf("%q is not a write mode: use upsert, insert or update", mode)
	}
	if err != nil {
		return WriteResult{}, fmt.Errorf("cannot %s into %q: %w", orUpsert(mode), h.Name, err)
	}

	if flush {
		if err := h.collection.Flush(); err != nil {
			return WriteResult{}, fmt.Errorf("wrote %d documents to %q but could not flush them to disk: %w", result.SuccessCount, h.Name, err)
		}
	}
	return WriteResult{Written: result.SuccessCount, Failed: result.ErrorCount}, nil
}

// Search is a nearest-neighbour search over one vector field.
type Search struct {
	// Field is the vector field to search. Required.
	Field string
	// Vector is the query vector. Required — text is embedded by the caller,
	// which owns the embedding provider.
	Vector []float32
	// TopK is how many documents to return.
	TopK int
	// Filter is a filter expression, evaluated before the search so it narrows
	// what is searched rather than what is returned.
	Filter string
	// OutputFields are the scalar fields to return; empty returns all of them.
	OutputFields []string
	// IncludeVector returns the stored vectors too, which are large.
	IncludeVector bool
	// IncludeDocID returns the engine's internal document ids.
	IncludeDocID bool
	// Params are the index's search-time knobs: ef for HNSW, nprobe for IVF
	// and IVF-RaBitQ, listSize for DiskANN, plus radius and the refiner flags.
	Params map[string]any
}

// Query runs a vector search.
func (h *Handle) Query(search Search) ([]Document, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.queryLocked(search)
}

// queryLocked is Query with the read lock already held, so a hybrid search
// that turns out to have only one half can fall back to it. Go's RWMutex is
// not re-entrant — a writer arriving between two RLocks would deadlock both —
// so the lock is taken once, at the top.
func (h *Handle) queryLocked(search Search) ([]Document, error) {
	if h.collection == nil {
		return nil, errClosed(h.Name)
	}
	if len(search.Vector) == 0 {
		return nil, fmt.Errorf("a vector search needs a query vector: give a query text to embed, or a vector")
	}
	if err := h.checkVectorField(search.Field, len(search.Vector)); err != nil {
		return nil, err
	}

	query := zvec.NewSearchQuery()
	if query == nil {
		return nil, fmt.Errorf("%w: cannot allocate a query", ErrLibrary)
	}
	defer query.Destroy()

	if err := query.SetFieldName(search.Field); err != nil {
		return nil, fmt.Errorf("cannot search the field %q: %w", search.Field, err)
	}
	if err := query.SetQueryVector(search.Vector); err != nil {
		return nil, fmt.Errorf("cannot set the query vector: %w", err)
	}
	if err := query.SetTopK(topK(search.TopK)); err != nil {
		return nil, fmt.Errorf("cannot set the result count: %w", err)
	}
	if err := applyCommon(query, search.Filter, search.OutputFields, search.IncludeVector); err != nil {
		return nil, err
	}
	if search.IncludeDocID {
		if err := query.SetIncludeDocID(true); err != nil {
			return nil, fmt.Errorf("cannot include document ids: %w", err)
		}
	}
	// Index params are applied to whichever index the field actually has, so a
	// form can offer one "search params" box rather than one per index type.
	release, err := applyQueryParams(queryTarget{search: query}, h.indexTypeOf(search.Field), search.Params)
	if err != nil {
		return nil, err
	}
	defer release()

	docs, err := h.collection.Query(query)
	if err != nil {
		return nil, searchError(h.Name, search.Filter, err)
	}
	return h.collect(docs, search.IncludeDocID), nil
}

// FTS is a full-text search over one text field.
type FTS struct {
	// Field is the full-text indexed field to match. Required.
	Field string
	// Match is a natural-language match string: the words to look for, scored
	// by relevance.
	Match string
	// Query is a boolean expression instead — "fox AND forest", "fox NOT dog".
	Query string
	// Operator is how adjacent bare words in Query are joined, "AND" or "OR".
	// Empty leaves the engine's own default.
	Operator      string
	TopK          int
	Filter        string
	OutputFields  []string
	IncludeVector bool
	IncludeDocID  bool
}

// QueryFTS runs a full-text search.
func (h *Handle) QueryFTS(search FTS) ([]Document, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.queryFTSLocked(search)
}

// queryFTSLocked is QueryFTS with the read lock already held. See queryLocked.
func (h *Handle) queryFTSLocked(search FTS) ([]Document, error) {
	if h.collection == nil {
		return nil, errClosed(h.Name)
	}
	if strings.TrimSpace(search.Match) == "" && strings.TrimSpace(search.Query) == "" {
		return nil, fmt.Errorf("a full-text search needs something to look for: fill in Match text or a Query expression")
	}
	if err := h.checkTextField(search.Field); err != nil {
		return nil, err
	}

	query := zvec.NewSearchQuery()
	if query == nil {
		return nil, fmt.Errorf("%w: cannot allocate a query", ErrLibrary)
	}
	defer query.Destroy()

	if err := query.SetFieldName(search.Field); err != nil {
		return nil, fmt.Errorf("cannot search the field %q: %w", search.Field, err)
	}
	if err := query.SetTopK(topK(search.TopK)); err != nil {
		return nil, fmt.Errorf("cannot set the result count: %w", err)
	}
	if err := applyCommon(query, search.Filter, search.OutputFields, search.IncludeVector); err != nil {
		return nil, err
	}
	if search.IncludeDocID {
		if err := query.SetIncludeDocID(true); err != nil {
			return nil, fmt.Errorf("cannot include document ids: %w", err)
		}
	}
	if err := attachFTS(ftsTarget{search: query}, search.Match, search.Query, search.Operator); err != nil {
		return nil, err
	}

	docs, err := h.collection.Query(query)
	if err != nil {
		return nil, searchError(h.Name, search.Filter, err)
	}
	return h.collect(docs, search.IncludeDocID), nil
}

// Hybrid is a search that runs a vector half and a full-text half and fuses
// their rankings — the reason to reach for zvec over a plain vector store.
//
// Each half produces its own ranking of its own candidates, and the engine
// merges them: with RRF, by where a document placed in each list, which needs
// no comparable scores and is the right default; with weights, by scaling the
// raw scores, which is sharper when both halves score on a similar scale and
// the balance between them is known.
type Hybrid struct {
	// VectorField and Vector are the vector half. Leave Vector empty to run a
	// text-only search through this action.
	VectorField string
	Vector      []float32
	// VectorCandidates is how many documents the vector half contributes to
	// the fusion. It should exceed TopK — the halves rarely agree, so a half
	// that offers only TopK candidates leaves the fusion nothing to choose
	// between.
	VectorCandidates int
	// VectorParams are the vector index's search-time knobs.
	VectorParams map[string]any

	// TextField with Match or Query is the text half.
	TextField      string
	Match          string
	Query          string
	Operator       string
	TextCandidates int

	// Rerank is "rrf" or "weighted".
	Rerank string
	// RRFK is RRF's rank constant: the larger it is, the flatter the weight
	// given to the top of each list. 60 is the usual starting point.
	RRFK int
	// VectorWeight and TextWeight are used when Rerank is "weighted".
	VectorWeight float64
	TextWeight   float64

	TopK          int
	Filter        string
	OutputFields  []string
	IncludeVector bool
	IncludeDocID  bool
}

// QueryHybrid runs a hybrid search.
func (h *Handle) QueryHybrid(search Hybrid) ([]Document, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.collection == nil {
		return nil, errClosed(h.Name)
	}

	hasVector := len(search.Vector) > 0
	hasText := strings.TrimSpace(search.Match) != "" || strings.TrimSpace(search.Query) != ""
	if !hasVector && !hasText {
		return nil, fmt.Errorf("a hybrid search needs something to search with: give a query text (used by both halves), or a vector, or a full-text expression")
	}
	if hasVector {
		if err := h.checkVectorField(search.VectorField, len(search.Vector)); err != nil {
			return nil, err
		}
	}
	if hasText {
		if err := h.checkTextField(search.TextField); err != nil {
			return nil, err
		}
	}

	// The engine's multi-query fuses rankings, and refuses fewer than two
	// sub-queries — there would be nothing to fuse. So a hybrid node pointed at
	// a collection with only one searchable half, or given only one kind of
	// query, runs as that half's own search instead of failing.
	if !hasVector || !hasText {
		return h.oneHalfLocked(search, hasVector)
	}

	multi := zvec.NewMultiQuery()
	if multi == nil {
		return nil, fmt.Errorf("%w: cannot allocate a hybrid query", ErrLibrary)
	}
	defer multi.Destroy()

	if err := multi.SetTopK(topK(search.TopK)); err != nil {
		return nil, fmt.Errorf("cannot set the result count: %w", err)
	}
	if filter := strings.TrimSpace(search.Filter); filter != "" {
		if err := multi.SetFilter(filter); err != nil {
			return nil, filterError(filter, err)
		}
	}
	if fields := trimmed(search.OutputFields); len(fields) > 0 {
		if err := multi.SetOutputFields(fields); err != nil {
			return nil, fmt.Errorf("cannot ask for the fields %s: %w", strings.Join(fields, ", "), err)
		}
	}
	if search.IncludeVector {
		if err := multi.SetIncludeVector(true); err != nil {
			return nil, fmt.Errorf("cannot include the stored vectors: %w", err)
		}
	}

	// The halves are added in a fixed order — vector first, then text — because
	// weighted reranking addresses them by position.
	if err := h.addVectorHalf(multi, search); err != nil {
		return nil, err
	}
	if err := h.addTextHalf(multi, search); err != nil {
		return nil, err
	}
	if err := applyRerank(multi, search); err != nil {
		return nil, err
	}

	docs, err := h.collection.MultiQuery(multi)
	if err != nil {
		return nil, searchError(h.Name, search.Filter, err)
	}
	return h.collect(docs, search.IncludeDocID), nil
}

// oneHalfLocked runs a hybrid search that has only one half to run. Weighted
// reranking is refused earlier, because weights address the halves by position
// and there is no second one to weigh against.
func (h *Handle) oneHalfLocked(search Hybrid, vector bool) ([]Document, error) {
	if strings.EqualFold(strings.TrimSpace(search.Rerank), "weighted") {
		missing := "vector"
		if vector {
			missing = "full-text"
		}
		return nil, fmt.Errorf("weighted reranking needs both halves of the search, but the %s half has nothing to run: fill it in, or use RRF", missing)
	}
	if vector {
		return h.queryLocked(Search{
			Field:         search.VectorField,
			Vector:        search.Vector,
			TopK:          search.TopK,
			Filter:        search.Filter,
			OutputFields:  search.OutputFields,
			IncludeVector: search.IncludeVector,
			IncludeDocID:  search.IncludeDocID,
			Params:        search.VectorParams,
		})
	}
	return h.queryFTSLocked(FTS{
		Field:         search.TextField,
		Match:         search.Match,
		Query:         search.Query,
		Operator:      search.Operator,
		TopK:          search.TopK,
		Filter:        search.Filter,
		OutputFields:  search.OutputFields,
		IncludeVector: search.IncludeVector,
		IncludeDocID:  search.IncludeDocID,
	})
}

func (h *Handle) addVectorHalf(multi *zvec.MultiQuery, search Hybrid) error {
	sub := zvec.NewSubQuery()
	if sub == nil {
		return fmt.Errorf("%w: cannot allocate the vector half", ErrLibrary)
	}
	defer sub.Destroy()

	if err := sub.SetFieldName(search.VectorField); err != nil {
		return fmt.Errorf("cannot search the field %q: %w", search.VectorField, err)
	}
	if err := sub.SetQueryVector(search.Vector); err != nil {
		return fmt.Errorf("cannot set the query vector: %w", err)
	}
	if err := sub.SetNumCandidates(candidates(search.VectorCandidates, search.TopK)); err != nil {
		return fmt.Errorf("cannot set how many candidates the vector half contributes: %w", err)
	}
	release, err := applyQueryParams(queryTarget{sub: sub}, h.indexTypeOf(search.VectorField), search.VectorParams)
	if err != nil {
		return err
	}
	defer release()

	if err := multi.AddSubQuery(sub); err != nil {
		return fmt.Errorf("cannot add the vector half of the search: %w", err)
	}
	return nil
}

func (h *Handle) addTextHalf(multi *zvec.MultiQuery, search Hybrid) error {
	sub := zvec.NewSubQuery()
	if sub == nil {
		return fmt.Errorf("%w: cannot allocate the text half", ErrLibrary)
	}
	defer sub.Destroy()

	if err := sub.SetFieldName(search.TextField); err != nil {
		return fmt.Errorf("cannot search the field %q: %w", search.TextField, err)
	}
	if err := sub.SetNumCandidates(candidates(search.TextCandidates, search.TopK)); err != nil {
		return fmt.Errorf("cannot set how many candidates the text half contributes: %w", err)
	}
	if err := attachFTS(ftsTarget{sub: sub}, search.Match, search.Query, search.Operator); err != nil {
		return err
	}
	if err := multi.AddSubQuery(sub); err != nil {
		return fmt.Errorf("cannot add the text half of the search: %w", err)
	}
	return nil
}

// applyRerank sets the fusion strategy. With one half there is nothing to fuse,
// so RRF is left in place and weights are refused rather than silently ignored.
func applyRerank(multi *zvec.MultiQuery, search Hybrid) error {
	switch strings.ToLower(strings.TrimSpace(search.Rerank)) {
	case "", "rrf":
		rankConstant := search.RRFK
		if rankConstant <= 0 {
			rankConstant = 60
		}
		if err := multi.SetRerankRRF(rankConstant); err != nil {
			return fmt.Errorf("cannot set reciprocal rank fusion with k=%d: %w", rankConstant, err)
		}
		return nil
	case "weighted":
		weights := []float64{orWeight(search.VectorWeight), orWeight(search.TextWeight)}
		if err := multi.SetRerankWeighted(weights); err != nil {
			return fmt.Errorf("cannot set weighted reranking (vector %.3f, text %.3f): %w", weights[0], weights[1], err)
		}
		return nil
	default:
		return fmt.Errorf("%q is not a reranking strategy: use rrf or weighted", search.Rerank)
	}
}

// Fetch reads documents by their keys. Keys that are not there are simply
// absent from the result, which is what makes this usable as an existence test.
func (h *Handle) Fetch(keys []string, outputFields []string, includeVector bool) ([]Document, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.collection == nil {
		return nil, errClosed(h.Name)
	}
	keys = trimmed(keys)
	if len(keys) == 0 {
		return nil, fmt.Errorf("no keys to fetch: list the document keys to read")
	}

	docs, err := h.collection.Fetch(keys, &zvec.FetchOptions{
		OutputFields:  trimmed(outputFields),
		IncludeVector: includeVector,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot fetch from %q: %w", h.Name, err)
	}
	return h.collect(docs, false), nil
}

// Scan walks the collection in storage order, skipping the first `skip`
// documents and returning at most `limit`.
//
// The engine's iterator is a live cursor that cannot be handed back to a later
// run, so paging through a collection across several flow runs means skipping
// what was already seen. That re-reads the earlier pages, which is fine for a
// few thousand documents and the wrong tool for a million — use a filter on an
// indexed field to narrow the collection instead.
func (h *Handle) Scan(limit, skip int, outputFields []string, includeVector bool) ([]Document, bool, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.collection == nil {
		return nil, false, errClosed(h.Name)
	}
	if limit <= 0 {
		limit = 50
	}

	options := zvec.NewIteratorOptions()
	if options == nil {
		return nil, false, fmt.Errorf("%w: cannot allocate an iterator", ErrLibrary)
	}
	defer options.Destroy()
	if fields := trimmed(outputFields); len(fields) > 0 {
		if err := options.SetOutputFields(fields); err != nil {
			return nil, false, fmt.Errorf("cannot ask for the fields %s: %w", strings.Join(fields, ", "), err)
		}
	}
	if err := options.SetIncludeVector(includeVector); err != nil {
		return nil, false, fmt.Errorf("cannot set whether vectors are included: %w", err)
	}

	iterator, err := h.collection.CreateIterator(options)
	if err != nil {
		return nil, false, fmt.Errorf("cannot scan %q: %w", h.Name, err)
	}
	defer iterator.Close()

	out := make([]Document, 0, limit)
	for seen := 0; ; seen++ {
		doc, err := iterator.Next()
		if err == io.EOF {
			return out, false, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("cannot read the next document of %q: %w", h.Name, err)
		}
		if seen < skip {
			doc.Destroy()
			continue
		}
		out = append(out, h.fromDoc(doc, false))
		doc.Destroy()
		if len(out) >= limit {
			// One more document exists only if the iterator has one; ask, so
			// the flow can tell a full page from the last page.
			next, err := iterator.Next()
			if err == nil {
				next.Destroy()
				return out, true, nil
			}
			return out, false, nil
		}
	}
}

// Delete removes documents by key.
func (h *Handle) Delete(keys []string, flush bool) (WriteResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.collection == nil {
		return WriteResult{}, errClosed(h.Name)
	}
	keys = trimmed(keys)
	if len(keys) == 0 {
		return WriteResult{}, fmt.Errorf("no keys to delete")
	}

	result, err := h.collection.Delete(keys)
	if err != nil {
		return WriteResult{}, fmt.Errorf("cannot delete from %q: %w", h.Name, err)
	}
	if flush {
		if err := h.collection.Flush(); err != nil {
			return WriteResult{}, fmt.Errorf("deleted from %q but could not flush to disk: %w", h.Name, err)
		}
	}
	return WriteResult{Written: result.SuccessCount, Failed: result.ErrorCount}, nil
}

// DeleteByFilter removes every document a filter expression selects. The engine
// does not report how many that was.
func (h *Handle) DeleteByFilter(filter string, flush bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.collection == nil {
		return errClosed(h.Name)
	}
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return fmt.Errorf("no filter to delete by")
	}

	if err := h.collection.DeleteByFilter(filter); err != nil {
		return fmt.Errorf("cannot delete from %q by filter: %w", h.Name, filterReason(filter, err))
	}
	if flush {
		if err := h.collection.Flush(); err != nil {
			return fmt.Errorf("deleted from %q but could not flush to disk: %w", h.Name, err)
		}
	}
	return nil
}

// Stats is what a collection can say about itself.
type Stats struct {
	Documents uint64             `json:"documents"`
	Indexes   []IndexStat        `json:"indexes"`
	Fields    []FieldDescription `json:"fields,omitempty"`
}

// IndexStat is one index and how much of the collection it covers. A
// completeness below 1 means documents written since the last optimize are
// still being searched by brute force.
type IndexStat struct {
	Field        string  `json:"field"`
	Completeness float32 `json:"completeness"`
}

// FieldDescription is one field as the collection actually holds it.
type FieldDescription struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Index    string `json:"index,omitempty"`
	Dim      uint32 `json:"dim,omitempty"`
	Nullable bool   `json:"nullable"`
	Vector   bool   `json:"vector,omitempty"`
}

// Stats reports the collection's size, its indexes, and the fields it declares.
func (h *Handle) Stats() (Stats, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.collection == nil {
		return Stats{}, errClosed(h.Name)
	}

	raw, err := h.collection.GetStats()
	if err != nil {
		return Stats{}, fmt.Errorf("cannot read the statistics of %q: %w", h.Name, err)
	}
	out := Stats{Documents: raw.DocCount, Indexes: make([]IndexStat, 0, raw.IndexCount)}
	for i, name := range raw.IndexNames {
		stat := IndexStat{Field: name}
		if i < len(raw.IndexCompleteness) {
			stat.Completeness = raw.IndexCompleteness[i]
		}
		out.Indexes = append(out.Indexes, stat)
	}
	out.Fields = h.describeFields()
	return out, nil
}

// describeFields lists the fields the collection declares.
//
// The C API cannot enumerate a schema, so the names come from the sidecar this
// plugin wrote at creation, and from a sample document for a collection it did
// not create. Each name is then described from the live schema, so the types
// reported are the collection's own rather than the sidecar's record of them.
func (h *Handle) describeFields() []FieldDescription {
	names := make([]string, 0, 8)
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}

	if spec := h.Spec; spec != nil {
		add(spec.VectorField())
		add(spec.TextField())
		for _, field := range spec.Fields {
			add(field.Name)
		}
	}
	for _, name := range h.sampleFieldNames() {
		add(name)
	}

	out := make([]FieldDescription, 0, len(names))
	for _, name := range names {
		declared := h.fieldType(name)
		if !declared.known {
			continue
		}
		description := FieldDescription{
			Name:   name,
			Type:   declared.dataType.String(),
			Dim:    declared.dim,
			Vector: declared.vector,
		}
		if field := h.schema.GetField(name); field != nil {
			description.Nullable = field.IsNullable()
			if field.HasIndex() {
				description.Index = field.GetIndexType().String()
			}
		}
		out = append(out, description)
	}
	return out
}

// sampleFieldNames reads the field names off one document, which is the only
// way to learn them for a collection this plugin did not create. An empty
// collection has none to offer.
func (h *Handle) sampleFieldNames() []string {
	options := zvec.NewIteratorOptions()
	if options == nil {
		return nil
	}
	defer options.Destroy()
	if err := options.SetIncludeVector(true); err != nil {
		return nil
	}

	iterator, err := h.collection.CreateIterator(options)
	if err != nil {
		return nil
	}
	defer iterator.Close()

	doc, err := iterator.Next()
	if err != nil {
		return nil
	}
	defer doc.Destroy()
	names, err := doc.GetFieldNames()
	if err != nil {
		return nil
	}
	return names
}

// Maintain flushes the collection to disk and optimizes it: merging segments
// and finishing the indexes that writes left incomplete. Optimizing is the
// expensive half, and the reason this is its own action rather than something
// every write does.
func (h *Handle) Maintain(flush, optimize bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.collection == nil {
		return errClosed(h.Name)
	}
	if flush {
		if err := h.collection.Flush(); err != nil {
			return fmt.Errorf("cannot flush %q to disk: %w", h.Name, err)
		}
	}
	if optimize {
		if err := h.collection.Optimize(); err != nil {
			return fmt.Errorf("cannot optimize %q: %w", h.Name, err)
		}
	}
	return nil
}

// ------------------------------------------------------------- shared bits --

// collect converts and frees the engine's result documents, hoisting the
// collection's own vector and text fields onto each Document.
func (h *Handle) collect(docs []*zvec.Doc, includeDocID bool) []Document {
	defer zvec.FreeDocs(docs)

	vectorField, textField := h.Spec.VectorField(), h.Spec.TextField()
	out := make([]Document, 0, len(docs))
	for _, doc := range docs {
		if doc == nil {
			continue
		}
		converted := h.fromDoc(doc, includeDocID)
		converted.Hoist(vectorField, textField)
		out = append(out, converted)
	}
	return out
}

// checkVectorField makes sure a search names a field that exists, holds
// vectors, and holds vectors of the length being searched with. Each of those
// would otherwise surface as an engine error that does not say which of the
// three went wrong.
func (h *Handle) checkVectorField(name string, length int) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("no vector field named, and the collection %q does not record one: name the field holding the vectors", h.Name)
	}
	declared := h.fieldType(name)
	if !declared.known {
		return unknownField(name, "vector field")
	}
	if !declared.vector {
		return fmt.Errorf("the field %q holds %s, not vectors: name the collection's vector field", name, declared.dataType)
	}
	if declared.dim > 0 && length > 0 && int(declared.dim) != length {
		return fmt.Errorf("the query vector has %d dimensions but %q holds %d: embed the query with the same model the collection was filled with", length, name, declared.dim)
	}
	return nil
}

// checkTextField makes sure a full-text search names a field that exists and
// is actually full-text indexed — searching an unindexed string field returns
// nothing at all, which looks like missing data rather than a missing index.
func (h *Handle) checkTextField(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("no text field named, and the collection %q does not record one: name the field holding the text", h.Name)
	}
	declared := h.fieldType(name)
	if !declared.known {
		return unknownField(name, "text field")
	}
	if h.indexTypeOf(name) != zvec.IndexTypeFTS {
		return fmt.Errorf("the field %q has no full-text index, so there is nothing to match against: create the collection with a text field, or search it as a filter instead (%s LIKE '%%term%%')", name, name)
	}
	return nil
}

// indexTypeOf is the index on a field, or IndexTypeUndefined when it has none.
func (h *Handle) indexTypeOf(name string) zvec.IndexType {
	if h.schema == nil {
		return zvec.IndexTypeUndefined
	}
	field := h.schema.GetField(name)
	if field == nil || !field.HasIndex() {
		return zvec.IndexTypeUndefined
	}
	return field.GetIndexType()
}

// applyCommon sets the parts a vector and a full-text search share.
func applyCommon(query *zvec.SearchQuery, filter string, outputFields []string, includeVector bool) error {
	if filter = strings.TrimSpace(filter); filter != "" {
		if err := query.SetFilter(filter); err != nil {
			return filterError(filter, err)
		}
	}
	if fields := trimmed(outputFields); len(fields) > 0 {
		if err := query.SetOutputFields(fields); err != nil {
			return fmt.Errorf("cannot ask for the fields %s: %w", strings.Join(fields, ", "), err)
		}
	}
	if includeVector {
		if err := query.SetIncludeVector(true); err != nil {
			return fmt.Errorf("cannot include the stored vectors: %w", err)
		}
	}
	return nil
}

// ftsTarget is a search or a hybrid half — the two things an FTS payload can be
// attached to. The engine gives them the same two setters and no common
// interface, so the choice is made here once rather than at both call sites.
type ftsTarget struct {
	search *zvec.SearchQuery
	sub    *zvec.SubQuery
}

func (t ftsTarget) setFTS(fts *zvec.FTS) error {
	if t.search != nil {
		return t.search.SetFTS(fts)
	}
	return t.sub.SetFTS(fts)
}

func (t ftsTarget) setParams(params *zvec.FTSQueryParams) error {
	if t.search != nil {
		return t.search.SetFTSParams(params)
	}
	return t.sub.SetFTSParams(params)
}

// attachFTS builds the full-text payload and hands it to the query. Both the
// payload and the params are deep-copied by the engine, so both are released
// here.
func attachFTS(target ftsTarget, match, query, operator string) error {
	payload := zvec.NewFTS()
	if payload == nil {
		return fmt.Errorf("%w: cannot allocate a full-text query", ErrLibrary)
	}
	defer payload.Destroy()

	if match = strings.TrimSpace(match); match != "" {
		if err := payload.SetMatchString(match); err != nil {
			return fmt.Errorf("cannot set the match text %q: %w", match, err)
		}
	}
	if query = strings.TrimSpace(query); query != "" {
		if err := payload.SetQueryString(query); err != nil {
			return fmt.Errorf("cannot set the full-text expression %q: %w", query, err)
		}
	}
	if err := target.setFTS(payload); err != nil {
		return fmt.Errorf("cannot attach the full-text query: %w", err)
	}

	if operator = strings.ToUpper(strings.TrimSpace(operator)); operator != "" {
		if operator != "AND" && operator != "OR" {
			return fmt.Errorf("%q is not a default operator: use AND or OR", operator)
		}
		params := zvec.NewFTSQueryParams(operator)
		if params == nil {
			return fmt.Errorf("%w: cannot allocate full-text query params", ErrLibrary)
		}
		defer params.Destroy()
		if err := target.setParams(params); err != nil {
			return fmt.Errorf("cannot set the default operator %s: %w", operator, err)
		}
	}
	return nil
}

func topK(k int) int {
	if k <= 0 {
		return 10
	}
	return k
}

// candidates is how many documents one half of a hybrid search contributes.
// Defaulting to several times TopK matters: fusion can only reorder what it is
// given, so a half that offers exactly TopK candidates can contribute nothing
// the other half did not already find.
func candidates(n, k int) int {
	if n > 0 {
		return n
	}
	if scaled := topK(k) * 5; scaled > 50 {
		return scaled
	}
	return 50
}

func orWeight(w float64) float64 {
	if w <= 0 {
		return 0.5
	}
	return w
}

func orUpsert(mode WriteMode) WriteMode {
	if mode == "" {
		return WriteUpsert
	}
	return mode
}

func trimmed(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func errClosed(name string) error {
	return fmt.Errorf("the collection %q was closed while this action ran: run the action again", name)
}

// filterError explains a rejected filter expression, because the engine's
// parser error is an ANTLR token dump and the mistake is nearly always the
// same one: writing == where the expression language wants =.
func filterError(filter string, err error) error {
	return fmt.Errorf("the filter %q was rejected: %w", filter, filterReason(filter, err))
}

func filterReason(filter string, err error) error {
	switch {
	case strings.Contains(filter, "=="):
		return fmt.Errorf("%w — the filter language is SQL-like, so equality is a single = (category = 'bikes'), not ==", err)
	case strings.Contains(filter, "''"):
		return fmt.Errorf(`%w — a quote inside a value is escaped with a backslash (name = 'O\'Brien'), not by doubling it`, err)
	}
	return err
}

// searchError adds the filter to a failed search, since a filter naming a field
// with no invert index is the usual cause and the engine's message does not say
// which part of the query it objected to.
func searchError(name, filter string, err error) error {
	if filter = strings.TrimSpace(filter); filter != "" {
		return fmt.Errorf("the search of %q failed with the filter %q: %w", name, filter, filterReason(filter, err))
	}
	return fmt.Errorf("the search of %q failed: %w", name, err)
}
