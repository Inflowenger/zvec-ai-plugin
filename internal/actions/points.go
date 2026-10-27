package actions

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/Inflowenger/zvec-ai-plugin/internal/embed"
	"github.com/Inflowenger/zvec-ai-plugin/internal/zvecdb"
	"github.com/google/uuid"
)

// ------------------------------------------------------------------ write --

type upsertInput struct {
	target

	Key    string     `json:"key"`
	Text   string     `json:"text"`
	Vector any        `json:"vector"`
	Values []valueRow `json:"values"`

	Documents any `json:"documents"`

	Mode        string `json:"mode"`
	Flush       *bool  `json:"flush"`
	VectorField string `json:"vectorField"`
	TextField   string `json:"textField"`
}

// valueRow is one row of a key/value table: a field name and the text typed
// against it.
type valueRow struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (r *Registry) upsert() sdkv1.Action {
	return sdkv1.Action{
		Method:      "zvec.doc.upsert",
		Title:       "Upsert documents",
		Description: "Write documents into a collection, embedding their text when an embedding provider is configured.",
		Icon:        sdkv1.Icon{Icon: "mdi-database-import"},
		Form:        upsertForm,
		RequestHandler: onCollection(r, "writing documents", func(ctx context.Context, job *sdkv1.Job, e env, h *zvecdb.Handle, in upsertInput) (map[string]any, error) {
			vectorField := orDefault(in.VectorField, h.Spec.VectorField())
			textField := orDefault(in.TextField, h.Spec.TextField())

			docs, err := in.documents()
			if err != nil {
				return nil, err
			}
			if err := r.fillVectors(ctx, e, docs, vectorField); err != nil {
				return nil, err
			}

			job.Progress(70, sdkv1.Frame{Title: "writing documents", Content: fmt.Sprintf("%d into %s", len(docs), h.Name)})
			result, err := h.Write(zvecdb.WriteMode(in.Mode), docs, vectorField, textField, boolOr(in.Flush, true))
			if err != nil {
				return nil, err
			}

			keys := make([]string, 0, len(docs))
			for _, doc := range docs {
				keys = append(keys, doc.Key)
			}
			return map[string]any{
				"collection": h.Name,
				"written":    result.Written,
				"failed":     result.Failed,
				"keys":       keys,
			}, nil
		}),
	}
}

// documents reads the documents to write: the batch field when it is filled,
// and otherwise the single document the boxes above it describe.
func (in upsertInput) documents() ([]zvecdb.Document, error) {
	if !zvecdb.IsBlank(in.Documents) {
		return decodeDocuments(in.Documents)
	}

	doc := zvecdb.Document{Key: strings.TrimSpace(in.Key), Text: in.Text}
	if !zvecdb.IsBlank(in.Vector) {
		vector, err := zvecdb.Vector(in.Vector, "vector")
		if err != nil {
			return nil, err
		}
		doc.Vector = vector
	}
	fields, err := rowsToFields(in.Values, "values")
	if err != nil {
		return nil, err
	}
	doc.Fields = fields

	if doc.Text == "" && len(doc.Vector) == 0 && len(doc.Fields) == 0 {
		return nil, fmt.Errorf("there is nothing to write: fill in Text, a Vector, or some Field values — or paste a batch into Documents")
	}
	if doc.Key == "" {
		doc.Key = uuid.NewString()
	}
	return []zvecdb.Document{doc}, nil
}

// decodeDocuments reads the batch field. Each entry is a document in the same
// shape a search returns, so a result can be piped straight back into a write.
func decodeDocuments(raw any) ([]zvecdb.Document, error) {
	items, err := zvecdb.Array(raw, "documents")
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("\"documents\" is an empty list")
	}

	docs := make([]zvecdb.Document, 0, len(items))
	for i, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("\"documents\"[%d] must be an object, e.g. {\"key\": \"a1\", \"text\": \"…\", \"fields\": {…}}", i)
		}
		doc := zvecdb.Document{}
		if key, ok := stringOf(object, "key", "id", "pk"); ok {
			doc.Key = strings.TrimSpace(key)
		}
		if text, ok := stringOf(object, "text"); ok {
			doc.Text = text
		}
		if raw, ok := object["vector"]; ok && !zvecdb.IsBlank(raw) {
			vector, err := zvecdb.Vector(raw, fmt.Sprintf("documents[%d].vector", i))
			if err != nil {
				return nil, err
			}
			doc.Vector = vector
		}
		if fields, ok := object["fields"].(map[string]any); ok {
			doc.Fields = fields
		} else {
			// No "fields" wrapper: everything that is not one of this plugin's
			// own keys is a field, so a flat document works too.
			doc.Fields = map[string]any{}
			for key, value := range object {
				switch key {
				case "key", "id", "pk", "text", "vector", "fields", "score", "docId":
				default:
					doc.Fields[key] = value
				}
			}
		}
		if doc.Key == "" {
			doc.Key = uuid.NewString()
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// fillVectors embeds every document that carries text but no vector, in one
// call to the provider, so a batch of a hundred documents is one request.
//
// Text with no provider configured is not an error here: a collection may have
// a full-text field and no vectors at all, and that collection's documents are
// text with nothing to embed into. It becomes an error only when the collection
// does have a vector field, because then the document would be written without
// the vector that makes it findable.
func (r *Registry) fillVectors(ctx context.Context, e env, docs []zvecdb.Document, vectorField string) error {
	if vectorField == "" {
		return nil
	}

	var (
		texts     []string
		positions []int
	)
	for i, doc := range docs {
		if len(doc.Vector) == 0 && strings.TrimSpace(doc.Text) != "" {
			texts = append(texts, doc.Text)
			positions = append(positions, i)
		}
	}
	if len(texts) == 0 {
		return nil
	}
	if !e.emb.Enabled() {
		return fmt.Errorf("%w (document %q has text but no vector, and %q is waiting for one)", embed.ErrNoProvider, docs[positions[0]].Key, vectorField)
	}

	vectors, err := embed.Texts(ctx, e.emb, texts, embed.Document)
	if err != nil {
		return fmt.Errorf("cannot embed the documents: %w", err)
	}
	for i, at := range positions {
		docs[at].Vector = vectors[i]
	}
	return nil
}

// ----------------------------------------------------------------- search --

type searchInput struct {
	target

	Text   string `json:"text"`
	Vector any    `json:"vector"`
	TopK   int    `json:"topK"`

	Filter  string     `json:"filter"`
	Filters []valueRow `json:"filters"`

	OutputFields  []string `json:"outputFields"`
	IncludeVector bool     `json:"includeVector"`
	VectorField   string   `json:"vectorField"`
	SearchParams  any      `json:"searchParams"`
}

func (r *Registry) search() sdkv1.Action {
	return sdkv1.Action{
		Method:      "zvec.doc.search",
		Title:       "Vector search",
		Description: "Find the documents whose vectors are nearest the query, optionally narrowed by a filter.",
		Icon:        sdkv1.Icon{Icon: "mdi-vector-triangle"},
		Form:        searchForm,
		RequestHandler: onCollection(r, "searching", func(ctx context.Context, job *sdkv1.Job, e env, h *zvecdb.Handle, in searchInput) (map[string]any, error) {
			vector, err := r.embedQuery(ctx, e, in.Text, in.Vector)
			if err != nil {
				return nil, err
			}
			if len(vector) == 0 {
				return nil, fmt.Errorf("a vector search needs something to search with: fill in Query text (with an embedding provider configured) or a Query vector")
			}
			filter, err := buildFilter(in.Filter, in.Filters)
			if err != nil {
				return nil, err
			}
			params, err := zvecdb.Object(in.SearchParams, "searchParams")
			if err != nil {
				return nil, err
			}

			job.Progress(70, sdkv1.Frame{Title: "searching", Content: h.Name})
			docs, err := h.Query(zvecdb.Search{
				Field:         orDefault(in.VectorField, h.Spec.VectorField()),
				Vector:        vector,
				TopK:          in.TopK,
				Filter:        filter,
				OutputFields:  in.OutputFields,
				IncludeVector: in.IncludeVector,
				Params:        params,
			})
			if err != nil {
				return nil, err
			}
			return results(h, docs, filter), nil
		}),
	}
}

type ftsInput struct {
	target

	Match    string `json:"match"`
	Query    string `json:"query"`
	Operator string `json:"operator"`
	TopK     int    `json:"topK"`

	Filter  string     `json:"filter"`
	Filters []valueRow `json:"filters"`

	OutputFields  []string `json:"outputFields"`
	IncludeVector bool     `json:"includeVector"`
	TextField     string   `json:"textField"`
}

func (r *Registry) fullText() sdkv1.Action {
	return sdkv1.Action{
		Method:      "zvec.doc.fts",
		Title:       "Full-text search",
		Description: "Search the collection's text field by words — exact terms, names and codes an embedding tends to blur.",
		Icon:        sdkv1.Icon{Icon: "mdi-text-search"},
		Form:        ftsForm,
		RequestHandler: onCollection(r, "searching text", func(ctx context.Context, job *sdkv1.Job, e env, h *zvecdb.Handle, in ftsInput) (map[string]any, error) {
			filter, err := buildFilter(in.Filter, in.Filters)
			if err != nil {
				return nil, err
			}

			job.Progress(70, sdkv1.Frame{Title: "searching text", Content: h.Name})
			docs, err := h.QueryFTS(zvecdb.FTS{
				Field:         orDefault(in.TextField, h.Spec.TextField()),
				Match:         in.Match,
				Query:         in.Query,
				Operator:      in.Operator,
				TopK:          in.TopK,
				Filter:        filter,
				OutputFields:  in.OutputFields,
				IncludeVector: in.IncludeVector,
			})
			if err != nil {
				return nil, err
			}
			return results(h, docs, filter), nil
		}),
	}
}

type hybridInput struct {
	target

	Text string `json:"text"`
	TopK int    `json:"topK"`

	Filter  string     `json:"filter"`
	Filters []valueRow `json:"filters"`

	Rerank       string  `json:"rerank"`
	RRFK         int     `json:"rrfK"`
	VectorWeight float64 `json:"vectorWeight"`
	TextWeight   float64 `json:"textWeight"`

	Vector           any    `json:"vector"`
	VectorField      string `json:"vectorField"`
	VectorCandidates int    `json:"vectorCandidates"`
	SearchParams     any    `json:"searchParams"`

	Match          string `json:"match"`
	Query          string `json:"query"`
	Operator       string `json:"operator"`
	TextField      string `json:"textField"`
	TextCandidates int    `json:"textCandidates"`

	OutputFields  []string `json:"outputFields"`
	IncludeVector bool     `json:"includeVector"`
}

func (r *Registry) hybrid() sdkv1.Action {
	return sdkv1.Action{
		Method:      "zvec.doc.hybrid",
		Title:       "Hybrid search",
		Description: "Search by meaning and by words at once, fusing the two rankings with RRF or weights.",
		Icon:        sdkv1.Icon{Icon: "mdi-set-split"},
		Form:        hybridForm,
		RequestHandler: onCollection(r, "hybrid search", func(ctx context.Context, job *sdkv1.Job, e env, h *zvecdb.Handle, in hybridInput) (map[string]any, error) {
			filter, err := buildFilter(in.Filter, in.Filters)
			if err != nil {
				return nil, err
			}
			params, err := zvecdb.Object(in.SearchParams, "searchParams")
			if err != nil {
				return nil, err
			}

			// One query text drives both halves. The vector half embeds it; the
			// text half matches it as words — unless either was given its own
			// override, which is what the two groups on the form are for.
			match := in.Match
			if strings.TrimSpace(match) == "" && strings.TrimSpace(in.Query) == "" {
				match = in.Text
			}
			vectorText := in.Text
			if !zvecdb.IsBlank(in.Vector) {
				vectorText = ""
			}

			search := zvecdb.Hybrid{
				VectorField:      orDefault(in.VectorField, h.Spec.VectorField()),
				VectorCandidates: in.VectorCandidates,
				VectorParams:     params,
				TextField:        orDefault(in.TextField, h.Spec.TextField()),
				Match:            match,
				Query:            in.Query,
				Operator:         in.Operator,
				TextCandidates:   in.TextCandidates,
				Rerank:           in.Rerank,
				RRFK:             in.RRFK,
				VectorWeight:     in.VectorWeight,
				TextWeight:       in.TextWeight,
				TopK:             in.TopK,
				Filter:           filter,
				OutputFields:     in.OutputFields,
				IncludeVector:    in.IncludeVector,
			}

			// The vector half is skipped rather than failed when the collection
			// has no vector field or nothing to embed: a hybrid node pointed at
			// a text-only collection then degrades to a full-text search
			// instead of refusing to run.
			if search.VectorField != "" {
				vector, err := r.embedQuery(ctx, e, vectorText, in.Vector)
				if err != nil {
					return nil, err
				}
				search.Vector = vector
			}

			job.Progress(70, sdkv1.Frame{Title: "hybrid search", Content: h.Name + " — " + describeHalves(search)})
			docs, err := h.QueryHybrid(search)
			if err != nil {
				return nil, err
			}
			out := results(h, docs, filter)
			out["halves"] = describeHalves(search)
			out["rerank"] = orDefault(in.Rerank, "rrf")
			return out, nil
		}),
	}
}

// describeHalves says which halves actually ran, so a hybrid search that
// quietly became a one-sided one says so on the node and in its result.
func describeHalves(search zvecdb.Hybrid) string {
	halves := make([]string, 0, 2)
	if len(search.Vector) > 0 {
		halves = append(halves, "vector")
	}
	if strings.TrimSpace(search.Match) != "" || strings.TrimSpace(search.Query) != "" {
		halves = append(halves, "text")
	}
	if len(halves) == 0 {
		return "nothing to search with"
	}
	return strings.Join(halves, " + ")
}

// ------------------------------------------------------------------- read --

type fetchInput struct {
	target
	Keys          []string `json:"keys"`
	OutputFields  []string `json:"outputFields"`
	IncludeVector bool     `json:"includeVector"`
}

func (r *Registry) fetch() sdkv1.Action {
	return sdkv1.Action{
		Method:      "zvec.doc.fetch",
		Title:       "Fetch documents",
		Description: "Read documents by their keys.",
		Icon:        sdkv1.Icon{Icon: "mdi-database-export"},
		Form:        fetchForm,
		RequestHandler: onCollection(r, "fetching documents", func(ctx context.Context, job *sdkv1.Job, e env, h *zvecdb.Handle, in fetchInput) (map[string]any, error) {
			docs, err := h.Fetch(in.Keys, in.OutputFields, in.IncludeVector)
			if err != nil {
				return nil, err
			}

			found := make(map[string]bool, len(docs))
			for _, doc := range docs {
				found[doc.Key] = true
			}
			missing := make([]string, 0)
			for _, key := range in.Keys {
				if key = strings.TrimSpace(key); key != "" && !found[key] {
					missing = append(missing, key)
				}
			}
			return map[string]any{
				"collection": h.Name,
				"documents":  docs,
				"count":      len(docs),
				"missing":    missing,
			}, nil
		}),
	}
}

type scanInput struct {
	target
	Limit         int      `json:"limit"`
	Skip          int      `json:"skip"`
	OutputFields  []string `json:"outputFields"`
	IncludeVector bool     `json:"includeVector"`
}

func (r *Registry) scan() sdkv1.Action {
	return sdkv1.Action{
		Method:      "zvec.doc.scan",
		Title:       "Scan documents",
		Description: "Walk a collection in storage order, a page at a time.",
		Icon:        sdkv1.Icon{Icon: "mdi-format-list-bulleted"},
		Form:        scanForm,
		RequestHandler: onCollection(r, "scanning", func(ctx context.Context, job *sdkv1.Job, e env, h *zvecdb.Handle, in scanInput) (map[string]any, error) {
			docs, more, err := h.Scan(in.Limit, in.Skip, in.OutputFields, in.IncludeVector)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"collection": h.Name,
				"documents":  docs,
				"count":      len(docs),
				"skip":       in.Skip,
				// nextSkip is where the following run carries on from, so a
				// paging loop can point its Skip box at this field.
				"nextSkip": in.Skip + len(docs),
				"more":     more,
			}, nil
		}),
	}
}

type deleteInput struct {
	target
	Keys    []string   `json:"keys"`
	Filter  string     `json:"filter"`
	Filters []valueRow `json:"filters"`
	Flush   *bool      `json:"flush"`
}

func (r *Registry) deleteDocs() sdkv1.Action {
	return sdkv1.Action{
		Method:      "zvec.doc.delete",
		Title:       "Delete documents",
		Description: "Delete documents by their keys, or by a filter.",
		Icon:        sdkv1.Icon{Icon: "mdi-delete-sweep"},
		Form:        deleteForm,
		RequestHandler: onCollection(r, "deleting documents", func(ctx context.Context, job *sdkv1.Job, e env, h *zvecdb.Handle, in deleteInput) (map[string]any, error) {
			filter, err := buildFilter(in.Filter, in.Filters)
			if err != nil {
				return nil, err
			}
			keys := nonEmpty(in.Keys)
			flush := boolOr(in.Flush, true)

			// Keys and a filter together would be two deletes dressed as one,
			// and which ran first would decide what the other matched.
			switch {
			case len(keys) == 0 && filter == "":
				return nil, fmt.Errorf("nothing selected to delete: list the keys, or give a filter")
			case len(keys) > 0 && filter != "":
				return nil, fmt.Errorf("select documents either by key or by filter, not both: use two Delete nodes if you mean both")
			case len(keys) > 0:
				result, err := h.Delete(keys, flush)
				if err != nil {
					return nil, err
				}
				return map[string]any{
					"collection": h.Name,
					"deleted":    result.Written,
					"failed":     result.Failed,
					"keys":       keys,
				}, nil
			default:
				if err := h.DeleteByFilter(filter, flush); err != nil {
					return nil, err
				}
				// The engine does not report how many documents a filtered
				// delete removed, so the result says what was asked rather
				// than inventing a count.
				return map[string]any{
					"collection": h.Name,
					"deleted":    nil,
					"filter":     filter,
				}, nil
			}
		}),
	}
}

// ---------------------------------------------------------------- helpers --

// results is the shape every search commits: the documents, how many there
// were, and the filter that was actually applied — which is worth reporting
// because it may have been assembled from the filter rows.
func results(h *zvecdb.Handle, docs []zvecdb.Document, filter string) map[string]any {
	out := map[string]any{
		"collection": h.Name,
		"documents":  docs,
		"count":      len(docs),
	}
	if filter != "" {
		out["filter"] = filter
	}
	return out
}

// buildFilter combines the filter rows with any hand-written expression. Every
// row must match, and so must the expression, so they are joined with AND.
func buildFilter(expression string, rows []valueRow) (string, error) {
	parts := make([]string, 0, len(rows)+1)
	if expression = strings.TrimSpace(expression); expression != "" {
		// Parenthesised so an expression containing OR cannot swallow the rows
		// that were meant to narrow it further.
		if len(rows) > 0 {
			parts = append(parts, "("+expression+")")
		} else {
			parts = append(parts, expression)
		}
	}
	for i, row := range rows {
		name := strings.TrimSpace(row.Key)
		if name == "" {
			if strings.TrimSpace(row.Value) == "" {
				continue // an empty row the user added and left alone
			}
			return "", fmt.Errorf("filter row %d has a value but no field name", i+1)
		}
		value, err := literal(row.Value)
		if err != nil {
			return "", fmt.Errorf("filter row %d (%s): %w", i+1, name, err)
		}
		parts = append(parts, fmt.Sprintf("%s = %s", name, value))
	}
	return strings.Join(parts, " AND "), nil
}

// literal renders one typed-in value as a filter literal, reading it as the
// type it plainly denotes: true/false a boolean, a bare number a number, and
// anything else a quoted string. A value wrapped in double quotes is forced to
// stay a string, which is how "42" matches text rather than a number.
//
// It returns an error for the one value it cannot express; see quote.
func literal(value string) (string, error) {
	text := strings.TrimSpace(value)
	switch text {
	case "true", "false":
		return text, nil
	}
	if _, err := strconv.ParseFloat(text, 64); err == nil && text != "" {
		return text, nil
	}
	if len(text) >= 2 && strings.HasPrefix(text, `"`) && strings.HasSuffix(text, `"`) {
		value = text[1 : len(text)-1]
	}
	return quote(value)
}

// quote wraps a value as a filter string literal.
//
// The engine's expression language escapes a quote inside a literal with a
// backslash — not by doubling it, which is what SQL itself would do and what
// this originally got wrong. A backslash anywhere else is literal: 'a\b' means
// a\b, and there is no escape that produces a single backslash.
//
// That leaves exactly one value this cannot express: one ending in a backslash,
// where the closing quote would be escaped instead of closing the literal. It
// is refused rather than emitted, because a broken filter would either fail to
// parse or, worse, parse as something else.
func quote(value string) (string, error) {
	if strings.HasSuffix(value, `\`) {
		return "", fmt.Errorf("the value %q ends with a backslash, which the filter language cannot express inside a value: drop it, or filter with a LIKE pattern in the Filter box", value)
	}
	return "'" + strings.ReplaceAll(value, "'", `\'`) + "'", nil
}

// rowsToFields turns a key/value table into a document's fields, reading each
// value as the type it denotes so a number lands in a numeric field.
func rowsToFields(rows []valueRow, field string) (map[string]any, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	fields := make(map[string]any, len(rows))
	for i, row := range rows {
		name := strings.TrimSpace(row.Key)
		if name == "" {
			if strings.TrimSpace(row.Value) == "" {
				continue
			}
			return nil, fmt.Errorf("%s row %d has a value but no field name", field, i+1)
		}
		fields[name] = typed(row.Value)
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return fields, nil
}

// typed reads a typed-in value as the type it denotes. The collection's
// declared type has the final say when the document is written; this only
// decides what a bare "42" starts life as.
func typed(value string) any {
	text := strings.TrimSpace(value)
	switch text {
	case "true":
		return true
	case "false":
		return false
	case "":
		return value
	}
	if number, err := strconv.ParseFloat(text, 64); err == nil {
		return number
	}
	if len(text) >= 2 && strings.HasPrefix(text, `"`) && strings.HasSuffix(text, `"`) {
		return text[1 : len(text)-1]
	}
	return value
}

func stringOf(object map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if raw, ok := object[key]; ok {
			switch value := raw.(type) {
			case string:
				return value, true
			case nil:
			default:
				return fmt.Sprintf("%v", value), true
			}
		}
	}
	return "", false
}

func orDefault(value, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}

func nonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}
