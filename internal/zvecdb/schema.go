package zvecdb

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	zvec "github.com/zvec-ai/zvec-go"
)

// Spec is a collection's declared shape: at most one dense vector field, at
// most one full-text field, and any number of scalar fields to filter on.
//
// It is the plugin's own vocabulary rather than the engine's, for two reasons.
// A node author writes "int64" and "cosine", not an enum constant; and the spec
// is JSON, so it can be stored beside the collection (see Sidecar) and read
// back later to remember which field holds the vectors — something the engine's
// own schema cannot be asked, as its C API offers no way to enumerate fields.
type Spec struct {
	Vector            *VectorSpec `json:"vector,omitempty"`
	Text              *TextSpec   `json:"text,omitempty"`
	Fields            []FieldSpec `json:"fields,omitempty"`
	MaxDocsPerSegment uint64      `json:"maxDocsPerSegment,omitempty"`
}

// VectorSpec is the dense vector field a vector or hybrid search runs against.
type VectorSpec struct {
	Field string `json:"field"`
	Dim   uint32 `json:"dim"`
	// Metric is how distance is measured: cosine, l2, ip or mipsl2. It must
	// match how the embeddings were produced — cosine for every embedding
	// provider this plugin can call.
	Metric string `json:"metric,omitempty"`
	// Index is the ANN index to build: hnsw, ivf, flat, ivf_rabitq or diskann.
	Index string `json:"index,omitempty"`
	// Quantize compresses the stored vectors: fp16, int8 or int4. Empty keeps
	// them as full FP32.
	Quantize string `json:"quantize,omitempty"`
	// Params are the index's own tuning knobs, by the names used below.
	Params map[string]any `json:"params,omitempty"`
}

// TextSpec is the full-text field an FTS or hybrid search matches against.
type TextSpec struct {
	Field string `json:"field"`
	// Tokenizer is standard, whitespace, ngram or jieba. zvec has no
	// "default" tokenizer, whatever its own README suggests.
	Tokenizer string `json:"tokenizer,omitempty"`
	// Filters are applied to every token: lowercase, ascii_folding, stemmer.
	Filters []string `json:"filters,omitempty"`
	// Params are tokenizer and filter options — ngram_min, ngram_max,
	// token_chars, cut_mode, stemmer_lang, jieba_dict_dir, user_dict_path.
	Params map[string]any `json:"params,omitempty"`
}

// FieldSpec is one scalar field: something to store alongside a document and,
// when Filterable, to narrow a search by.
type FieldSpec struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Filterable builds an invert index on the field, which is what makes it
	// usable in a filter expression. Without one the field is still stored and
	// still returned, it just cannot be filtered on.
	Filterable bool `json:"filterable,omitempty"`
	// Wildcard additionally allows LIKE patterns on a filterable string field.
	Wildcard bool `json:"wildcard,omitempty"`
	// Required makes the field non-nullable, so every document must carry it.
	Required bool `json:"required,omitempty"`
}

// scalarTypes maps this plugin's type names onto the engine's data types. Only
// the scalar types a document can carry are here: vectors are declared through
// VectorSpec, which knows about dimensions.
var scalarTypes = map[string]zvec.DataType{
	"string": zvec.DataTypeString,
	"bool":   zvec.DataTypeBool,
	"int32":  zvec.DataTypeInt32,
	"int64":  zvec.DataTypeInt64,
	"uint32": zvec.DataTypeUint32,
	"uint64": zvec.DataTypeUint64,
	"float":  zvec.DataTypeFloat,
	"double": zvec.DataTypeDouble,
	"binary": zvec.DataTypeBinary,
}

// ScalarTypeNames lists the type names a scalar field may declare, for a form's
// drop-down and for an error that has to say what was allowed.
func ScalarTypeNames() []string {
	names := make([]string, 0, len(scalarTypes))
	for name := range scalarTypes {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

var metrics = map[string]zvec.MetricType{
	"cosine": zvec.MetricTypeCosine,
	"l2":     zvec.MetricTypeL2,
	"ip":     zvec.MetricTypeIP,
	"mipsl2": zvec.MetricTypeMIPSL2,
}

var quantizers = map[string]zvec.QuantizeType{
	"fp16": zvec.QuantizeTypeFP16,
	"int8": zvec.QuantizeTypeInt8,
	"int4": zvec.QuantizeTypeInt4,
}

// IndexNames are the vector index types a collection may be created with.
var IndexNames = []string{"hnsw", "ivf", "flat", "ivf_rabitq", "diskann"}

// TokenizerNames are the FTS tokenizers the engine accepts.
var TokenizerNames = []string{"standard", "whitespace", "ngram", "jieba"}

// TokenFilterNames are the FTS token filters the engine accepts.
var TokenFilterNames = []string{"lowercase", "ascii_folding", "stemmer"}

// Validate reports what would make the spec unbuildable, in the words of the
// form the user filled in.
func (s *Spec) Validate() error {
	if s.Vector == nil && s.Text == nil {
		return fmt.Errorf("a collection needs a vector field, a text field, or both: fill in at least one")
	}
	named := map[string]bool{}
	claim := func(what, name string) error {
		switch {
		case strings.TrimSpace(name) == "":
			return fmt.Errorf("the %s has no name", what)
		case named[name]:
			return fmt.Errorf("two fields are both called %q: every field name must be distinct", name)
		}
		named[name] = true
		return nil
	}

	if v := s.Vector; v != nil {
		if err := claim("vector field", v.Field); err != nil {
			return err
		}
		if v.Dim == 0 {
			return fmt.Errorf("the vector field %q has no dimension: set it to the size of the vectors it will hold (an embedding model's dimension — use Test embedding in the settings dialog to read it off)", v.Field)
		}
		if _, err := v.metric(); err != nil {
			return err
		}
		if _, err := v.quantize(); err != nil {
			return err
		}
		if name := v.indexName(); !slices.Contains(IndexNames, name) {
			return fmt.Errorf("%q is not a vector index type: use one of %s", v.Index, strings.Join(IndexNames, ", "))
		}
	}
	if t := s.Text; t != nil {
		if err := claim("text field", t.Field); err != nil {
			return err
		}
		if name := t.tokenizerName(); !slices.Contains(TokenizerNames, name) {
			return fmt.Errorf("%q is not a tokenizer: use one of %s", t.Tokenizer, strings.Join(TokenizerNames, ", "))
		}
		for _, filter := range t.Filters {
			if filter = strings.TrimSpace(filter); filter != "" && !slices.Contains(TokenFilterNames, filter) {
				return fmt.Errorf("%q is not a token filter: use one of %s", filter, strings.Join(TokenFilterNames, ", "))
			}
		}
	}
	for i, field := range s.Fields {
		if err := claim(fmt.Sprintf("field %d", i+1), field.Name); err != nil {
			return err
		}
		if _, ok := scalarTypes[strings.ToLower(strings.TrimSpace(field.Type))]; !ok {
			return fmt.Errorf("field %q has type %q: use one of %s", field.Name, field.Type, strings.Join(ScalarTypeNames(), ", "))
		}
	}
	return nil
}

// Build turns the spec into an engine schema. The caller destroys the result.
//
// Every handle made along the way is released here, because the engine clones a
// field when it is added and deep-copies index params when they are set — so
// holding them any longer only leaks.
func (s *Spec) Build(name string) (*zvec.CollectionSchema, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}

	schema := zvec.NewCollectionSchema(name)
	if schema == nil {
		return nil, fmt.Errorf("%w: cannot allocate a schema", ErrLibrary)
	}
	fail := func(err error) (*zvec.CollectionSchema, error) {
		schema.Destroy()
		return nil, err
	}

	if v := s.Vector; v != nil {
		params, err := v.indexParams()
		if err != nil {
			return fail(err)
		}
		if err := addField(schema, v.Field, zvec.DataTypeVectorFP32, false, v.Dim, params); err != nil {
			return fail(err)
		}
	}
	if t := s.Text; t != nil {
		params, err := t.indexParams()
		if err != nil {
			return fail(err)
		}
		// A text field is nullable so a document may carry a vector and no
		// text, which is the ordinary case in a hybrid collection.
		if err := addField(schema, t.Field, zvec.DataTypeString, true, 0, params); err != nil {
			return fail(err)
		}
	}
	for _, field := range s.Fields {
		dataType := scalarTypes[strings.ToLower(strings.TrimSpace(field.Type))]
		var params *zvec.IndexParams
		if field.Filterable {
			built, err := zvec.NewInvertIndexParams(true, field.Wildcard)
			if err != nil {
				return fail(fmt.Errorf("cannot make field %q filterable: %w", field.Name, err))
			}
			params = built
		}
		if err := addField(schema, field.Name, dataType, !field.Required, 0, params); err != nil {
			return fail(err)
		}
	}

	if s.MaxDocsPerSegment > 0 {
		if err := schema.SetMaxDocCountPerSegment(s.MaxDocsPerSegment); err != nil {
			return fail(fmt.Errorf("cannot set the segment size: %w", err))
		}
	}
	return schema, nil
}

// addField adds one field and releases both handles it borrowed. params may be
// nil, for a field that is stored but not indexed.
func addField(schema *zvec.CollectionSchema, name string, dataType zvec.DataType, nullable bool, dim uint32, params *zvec.IndexParams) error {
	if params != nil {
		defer params.Destroy()
	}
	field := zvec.NewFieldSchema(name, dataType, nullable, dim)
	if field == nil {
		return fmt.Errorf("%w: cannot allocate the field %q", ErrLibrary, name)
	}
	defer field.Destroy()

	if params != nil {
		if err := field.SetIndexParams(params); err != nil {
			return fmt.Errorf("cannot index the field %q: %w", name, err)
		}
	}
	if err := schema.AddField(field); err != nil {
		return fmt.Errorf("cannot add the field %q: %w", name, err)
	}
	return nil
}

func (v *VectorSpec) indexName() string {
	if name := strings.ToLower(strings.TrimSpace(v.Index)); name != "" {
		return name
	}
	return "hnsw"
}

func (v *VectorSpec) metric() (zvec.MetricType, error) {
	name := strings.ToLower(strings.TrimSpace(v.Metric))
	if name == "" {
		return zvec.MetricTypeCosine, nil
	}
	metric, ok := metrics[name]
	if !ok {
		return 0, fmt.Errorf("%q is not a distance metric: use cosine, l2, ip or mipsl2", v.Metric)
	}
	return metric, nil
}

func (v *VectorSpec) quantize() (zvec.QuantizeType, error) {
	name := strings.ToLower(strings.TrimSpace(v.Quantize))
	if name == "" || name == "none" {
		return zvec.QuantizeTypeUndefined, nil
	}
	quantize, ok := quantizers[name]
	if !ok {
		return 0, fmt.Errorf("%q is not a quantization: use fp16, int8, int4, or leave it empty for full precision", v.Quantize)
	}
	return quantize, nil
}

// indexParams builds the vector index. The defaults are the ones zvec's own
// examples use, so a spec that names only a dimension still gets a sensible
// HNSW graph.
func (v *VectorSpec) indexParams() (*zvec.IndexParams, error) {
	metric, err := v.metric()
	if err != nil {
		return nil, err
	}
	quantize, err := v.quantize()
	if err != nil {
		return nil, err
	}
	p := params(v.Params)

	var built *zvec.IndexParams
	switch name := v.indexName(); name {
	case "hnsw":
		built, err = zvec.NewHNSWIndexParams(metric, p.intOr("m", 16), p.intOr("efConstruction", 200))
	case "ivf":
		built, err = zvec.NewIVFIndexParams(metric, p.intOr("nlist", 128), p.intOr("nIters", 10), p.boolOr("useSoar", false))
	case "flat":
		built, err = zvec.NewFlatIndexParams(metric)
	case "ivf_rabitq":
		built, err = zvec.NewIVFRaBitQIndexParams(metric, p.intOr("nlist", 128), p.intOr("totalBits", 1), p.intOr("sampleCount", 0))
	case "diskann":
		built, err = zvec.NewDiskANNIndexParams(metric, p.intOr("maxDegree", 64), p.intOr("listSize", 100), p.intOr("pqChunkNum", 0))
	default:
		return nil, fmt.Errorf("%q is not a vector index type: use one of %s", name, strings.Join(IndexNames, ", "))
	}
	if err != nil {
		return nil, fmt.Errorf("cannot configure the %s index: %w", v.indexName(), err)
	}
	if err := p.err(); err != nil {
		built.Destroy()
		return nil, fmt.Errorf("%s index params: %w", v.indexName(), err)
	}

	if quantize != zvec.QuantizeTypeUndefined {
		if err := built.SetQuantizeType(quantize); err != nil {
			built.Destroy()
			return nil, fmt.Errorf("cannot quantize to %s: %w", v.Quantize, err)
		}
	}
	return built, nil
}

func (t *TextSpec) tokenizerName() string {
	if name := strings.ToLower(strings.TrimSpace(t.Tokenizer)); name != "" {
		return name
	}
	return "standard"
}

// indexParams builds the FTS index. The engine takes the tokenizer and filter
// options as a JSON object string, which is what Params is rendered into.
func (t *TextSpec) indexParams() (*zvec.IndexParams, error) {
	extra := ""
	if len(t.Params) > 0 {
		encoded, err := json.Marshal(t.Params)
		if err != nil {
			return nil, fmt.Errorf("the tokenizer options are not encodable as JSON: %w", err)
		}
		extra = string(encoded)
	}

	filters := make([]string, 0, len(t.Filters))
	for _, filter := range t.Filters {
		if filter = strings.TrimSpace(filter); filter != "" {
			filters = append(filters, filter)
		}
	}

	built, err := zvec.NewFTSIndexParams(t.tokenizerName(), filters, extra)
	if err != nil {
		return nil, fmt.Errorf("cannot configure full-text search on %q: %w", t.Field, err)
	}
	return built, nil
}

// VectorField is the declared vector field name, or "" when the collection has
// no vector field.
func (s *Spec) VectorField() string {
	if s == nil || s.Vector == nil {
		return ""
	}
	return s.Vector.Field
}

// TextField is the declared full-text field name, or "" when there is none.
func (s *Spec) TextField() string {
	if s == nil || s.Text == nil {
		return ""
	}
	return s.Text.Field
}
