package actions

import (
	"context"
	"fmt"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/Inflowenger/zvec-ai-plugin/internal/zvecdb"
)

// createInput is the Create collection form. It is the plugin's vocabulary for
// a schema — one vector field, one text field, and the scalar fields to filter
// on — which zvecdb.Spec then turns into the engine's.
type createInput struct {
	target

	VectorField string `json:"vectorField"`
	Dim         int    `json:"dim"`
	Metric      string `json:"metric"`
	Index       string `json:"index"`
	Quantize    string `json:"quantize"`
	IndexParams any    `json:"indexParams"`

	TextField       string   `json:"textField"`
	Tokenizer       string   `json:"tokenizer"`
	TokenFilters    []string `json:"tokenFilters"`
	TokenizerParams any      `json:"tokenizerParams"`

	Fields []fieldRow `json:"fields"`

	MaxDocsPerSegment int `json:"maxDocsPerSegment"`
	// IfNotExists is a pointer so a batch body that omits it still gets the
	// form's default of true, rather than Go's zero value of false.
	IfNotExists *bool `json:"ifNotExists"`
}

// fieldRow is one row of the Create form's field table.
type fieldRow struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Filterable defaults to true for the same reason as IfNotExists: a field
	// nobody can filter on is rarely what someone adding a field meant.
	Filterable *bool `json:"filterable"`
	Required   bool  `json:"required"`
	Wildcard   bool  `json:"wildcard"`
}

func (r *Registry) createCollection() sdkv1.Action {
	return sdkv1.Action{
		Method:      "zvec.collection.create",
		Title:       "Create collection",
		Description: "Create a zvec collection with a vector field, a full-text field, or both.",
		Icon:        sdkv1.Icon{Icon: "mdi-database-plus"},
		Form:        createForm,
		RequestHandler: run(r, "creating collection", func(ctx context.Context, job *sdkv1.Job, e env, in createInput) (map[string]any, error) {
			spec, err := in.spec()
			if err != nil {
				return nil, err
			}
			handle, created, err := r.store.Create(e.cfg, in.Collection, spec, boolOr(in.IfNotExists, true))
			if err != nil {
				return nil, err
			}

			out := map[string]any{
				"collection":  handle.Name,
				"created":     created,
				"path":        handle.Path,
				"vectorField": spec.VectorField(),
				"textField":   spec.TextField(),
			}
			if !created {
				// The collection was already there and was left alone, which is
				// a success — but the schema in the form is not necessarily the
				// schema on disk, so say which one the result describes.
				out["vectorField"] = handle.Spec.VectorField()
				out["textField"] = handle.Spec.TextField()
				out["note"] = "the collection already existed and was left as it was"
			}
			return out, nil
		}),
	}
}

// spec turns the form into a collection schema. A vector field is declared
// when it is named, a text field when it is named, and the action refuses a
// collection with neither — there would be nothing to search.
func (in createInput) spec() (*zvecdb.Spec, error) {
	spec := &zvecdb.Spec{MaxDocsPerSegment: uint64(max(in.MaxDocsPerSegment, 0))}

	if field := strings.TrimSpace(in.VectorField); field != "" {
		params, err := zvecdb.Object(in.IndexParams, "indexParams")
		if err != nil {
			return nil, err
		}
		if in.Dim <= 0 {
			return nil, fmt.Errorf("the vector field %q needs a vector size: set it to the dimension of your embedding model (press ↻ on the settings dialog's Model box to read it off), or clear the field name for a text-only collection", field)
		}
		spec.Vector = &zvecdb.VectorSpec{
			Field:    field,
			Dim:      uint32(in.Dim),
			Metric:   in.Metric,
			Index:    in.Index,
			Quantize: in.Quantize,
			Params:   params,
		}
	}
	if field := strings.TrimSpace(in.TextField); field != "" {
		params, err := zvecdb.Object(in.TokenizerParams, "tokenizerParams")
		if err != nil {
			return nil, err
		}
		spec.Text = &zvecdb.TextSpec{
			Field:     field,
			Tokenizer: in.Tokenizer,
			Filters:   in.TokenFilters,
			Params:    params,
		}
	}

	for _, row := range in.Fields {
		if strings.TrimSpace(row.Name) == "" {
			continue
		}
		spec.Fields = append(spec.Fields, zvecdb.FieldSpec{
			Name:       strings.TrimSpace(row.Name),
			Type:       row.Type,
			Filterable: boolOr(row.Filterable, true),
			Wildcard:   row.Wildcard,
			Required:   row.Required,
		})
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return spec, nil
}

func (r *Registry) listCollections() sdkv1.Action {
	return sdkv1.Action{
		Method:      "zvec.collection.list",
		Title:       "List collections",
		Description: "Name every collection in the profile's data directory.",
		Icon:        sdkv1.Icon{Icon: "mdi-database-search"},
		Form:        listForm,
		RequestHandler: run(r, "listing collections", func(ctx context.Context, job *sdkv1.Job, e env, in struct{}) (map[string]any, error) {
			names, err := e.cfg.ListCollections()
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"collections": names,
				"count":       len(names),
				"dataDir":     e.cfg.DataDir,
			}, nil
		}),
	}
}

type infoInput struct{ target }

func (r *Registry) collectionInfo() sdkv1.Action {
	return sdkv1.Action{
		Method:      "zvec.collection.info",
		Title:       "Collection info",
		Description: "Document count, index completeness and the fields a collection declares.",
		Icon:        sdkv1.Icon{Icon: "mdi-information-outline"},
		Form:        infoForm,
		RequestHandler: onCollection(r, "reading collection", func(ctx context.Context, job *sdkv1.Job, e env, h *zvecdb.Handle, in infoInput) (map[string]any, error) {
			stats, err := h.Stats()
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"collection":  h.Name,
				"path":        h.Path,
				"documents":   stats.Documents,
				"indexes":     stats.Indexes,
				"fields":      stats.Fields,
				"vectorField": h.Spec.VectorField(),
				"textField":   h.Spec.TextField(),
			}, nil
		}),
	}
}

type optimizeInput struct {
	target
	Flush    *bool `json:"flush"`
	Optimize *bool `json:"optimize"`
}

func (r *Registry) optimizeCollection() sdkv1.Action {
	return sdkv1.Action{
		Method:      "zvec.collection.optimize",
		Title:       "Optimize collection",
		Description: "Flush pending writes and merge segments, finishing the indexes a bulk load left incomplete.",
		Icon:        sdkv1.Icon{Icon: "mdi-cog-sync"},
		Form:        optimizeForm,
		RequestHandler: onCollection(r, "optimizing", func(ctx context.Context, job *sdkv1.Job, e env, h *zvecdb.Handle, in optimizeInput) (map[string]any, error) {
			flush, optimize := boolOr(in.Flush, true), boolOr(in.Optimize, true)
			job.Progress(60, sdkv1.Frame{Title: "optimizing", Content: h.Name + " — this can take a while on a large collection"})
			if err := h.Maintain(flush, optimize); err != nil {
				return nil, err
			}

			stats, err := h.Stats()
			if err != nil {
				// The maintenance itself worked; not being able to read the
				// statistics afterwards should not fail the node.
				return map[string]any{"collection": h.Name, "flushed": flush, "optimized": optimize}, nil
			}
			return map[string]any{
				"collection": h.Name,
				"flushed":    flush,
				"optimized":  optimize,
				"documents":  stats.Documents,
				"indexes":    stats.Indexes,
			}, nil
		}),
	}
}

type dropInput struct {
	target
	Confirm bool `json:"confirm"`
}

func (r *Registry) dropCollection() sdkv1.Action {
	return sdkv1.Action{
		Method:      "zvec.collection.drop",
		Title:       "Drop collection",
		Description: "Delete a collection and every document in it. This cannot be undone.",
		Icon:        sdkv1.Icon{Icon: "mdi-database-remove"},
		Form:        dropForm,
		RequestHandler: run(r, "dropping collection", func(ctx context.Context, job *sdkv1.Job, e env, in dropInput) (map[string]any, error) {
			name, err := zvecdb.CheckName(in.Collection)
			if err != nil {
				return nil, err
			}
			// The confirmation is checked before anything is opened, so a node
			// left unticked never even takes the collection's lock.
			if !in.Confirm {
				return nil, fmt.Errorf("refusing to drop %q: tick \"Yes, delete it\" on the node to confirm, because this deletes the collection's data from disk and cannot be undone", name)
			}
			if err := r.store.Drop(e.cfg, name); err != nil {
				return nil, err
			}
			return map[string]any{"collection": name, "dropped": true}, nil
		}),
	}
}

// boolOr reads a tri-state checkbox: set to what the form said, or to the
// default when the field was absent.
func boolOr(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}
