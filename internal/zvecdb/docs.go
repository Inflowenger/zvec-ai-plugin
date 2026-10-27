package zvecdb

import (
	"encoding/base64"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	zvec "github.com/zvec-ai/zvec-go"
)

// Document is one record as a flow sees it: a key, the fields it carries, and —
// on the way out of a search — what the search thought of it.
//
// It is deliberately flat and JSON-shaped, because it is both what an upsert
// accepts and what a search commits, so a search result can be fed straight
// back into an upsert.
type Document struct {
	// Key is the document's primary key. An upsert generates one when it is
	// left empty.
	Key string `json:"key"`
	// Text is the content to store in the collection's full-text field and,
	// when no vector is given, to embed into its vector field.
	Text string `json:"text,omitempty"`
	// Vector is the dense vector to store, when it is supplied rather than
	// embedded from Text.
	Vector []float32 `json:"vector,omitempty"`
	// Fields are the scalar fields, by the names the collection declared.
	Fields map[string]any `json:"fields,omitempty"`
	// Score is the search score, present only on results. Its meaning follows
	// the metric: for cosine and L2 a smaller distance is nearer, for a
	// full-text match a larger score is better.
	Score float32 `json:"score,omitempty"`
	// DocID is the engine's internal document id, present only when a search
	// was asked to include it.
	DocID uint64 `json:"docId,omitempty"`
}

// fieldType resolves one field's declared type, caching it. The C API has no
// way to enumerate a schema's fields, so a type is looked up by name the first
// time a document mentions it, and an unknown name is cached too — a flow that
// repeatedly sends the same wrong field should not re-ask on every document.
func (h *Handle) fieldType(name string) fieldType {
	h.typesMu.Lock()
	defer h.typesMu.Unlock()

	if cached, ok := h.types[name]; ok {
		return cached
	}
	resolved := fieldType{}
	if h.schema != nil {
		if field := h.schema.GetField(name); field != nil {
			resolved = fieldType{
				dataType: field.GetDataType(),
				dim:      field.GetDimension(),
				vector:   field.IsVectorField(),
				known:    true,
			}
		}
	}
	h.types[name] = resolved
	return resolved
}

// HasField reports whether the collection declares a field.
func (h *Handle) HasField(name string) bool {
	return h.fieldType(name).known
}

// toDoc builds an engine document from a Document, using the collection's
// declared types to pick how each value is stored. The caller destroys it.
//
// vectorField and textField name where Vector and Text belong; either may be
// empty when the collection has no such field.
func (h *Handle) toDoc(in Document, vectorField, textField string) (*zvec.Doc, error) {
	doc := zvec.NewDoc()
	if doc == nil {
		return nil, fmt.Errorf("%w: cannot allocate a document", ErrLibrary)
	}
	fail := func(err error) (*zvec.Doc, error) {
		doc.Destroy()
		return nil, err
	}

	if in.Key == "" {
		return fail(fmt.Errorf("a document has no key: give one, or leave the Key box empty for a generated id"))
	}
	doc.SetPK(in.Key)

	if len(in.Vector) > 0 {
		if vectorField == "" {
			return fail(fmt.Errorf("document %q carries a vector, but no vector field was named and the collection does not declare one", in.Key))
		}
		declared := h.fieldType(vectorField)
		if !declared.known {
			return fail(unknownField(vectorField, "vector field"))
		}
		if declared.dim > 0 && int(declared.dim) != len(in.Vector) {
			return fail(fmt.Errorf("document %q has a %d-dimensional vector but %q holds %d dimensions: embed with the model the collection was sized for", in.Key, len(in.Vector), vectorField, declared.dim))
		}
		if err := doc.AddVectorFP32Field(vectorField, in.Vector); err != nil {
			return fail(fmt.Errorf("cannot set %q on document %q: %w", vectorField, in.Key, err))
		}
	}

	if in.Text != "" && textField != "" {
		if !h.fieldType(textField).known {
			return fail(unknownField(textField, "text field"))
		}
		if err := doc.AddStringField(textField, in.Text); err != nil {
			return fail(fmt.Errorf("cannot set %q on document %q: %w", textField, in.Key, err))
		}
	}

	// Fields are written in name order so that, whatever a flow's JSON
	// iteration order was, the same document always produces the same writes.
	for _, name := range sortedFieldNames(in.Fields) {
		// Vector and Text have already written their fields above; a second
		// write of the same name would make the winner depend on map order.
		if writtenAlready(in, name, vectorField, textField) {
			continue
		}
		if err := h.addField(doc, in.Key, name, in.Fields[name]); err != nil {
			return fail(err)
		}
	}
	return doc, nil
}

// writtenAlready reports whether Vector or Text has already supplied a field,
// so an entry of the same name in Fields is skipped rather than fighting it.
func writtenAlready(in Document, name, vectorField, textField string) bool {
	switch {
	case name == vectorField && len(in.Vector) > 0:
		return true
	case name == textField && in.Text != "":
		return true
	default:
		return false
	}
}

// addField writes one scalar value, converting it to the type the collection
// declared. The declared type decides, not the JSON type, so a 7 lands in an
// int64 field as an integer and in a double field as a double.
func (h *Handle) addField(doc *zvec.Doc, key, name string, value any) error {
	declared := h.fieldType(name)
	if !declared.known {
		return unknownField(name, fmt.Sprintf("field on document %q", key))
	}

	if value == nil {
		if err := doc.SetFieldNull(name); err != nil {
			return fmt.Errorf("cannot set %q to null on document %q: %w (a field must be nullable to hold null)", name, key, err)
		}
		return nil
	}

	var err error
	switch declared.dataType {
	case zvec.DataTypeString:
		err = doc.AddStringField(name, toString(value))
	case zvec.DataTypeBool:
		parsed, perr := toBool(value)
		if perr != nil {
			return fieldError(key, name, value, "true or false")
		}
		err = doc.AddBoolField(name, parsed)
	case zvec.DataTypeInt32:
		parsed, perr := toInt(value)
		if perr != nil || parsed > math.MaxInt32 || parsed < math.MinInt32 {
			return fieldError(key, name, value, "a 32-bit whole number")
		}
		err = doc.AddInt32Field(name, int32(parsed))
	case zvec.DataTypeInt64:
		parsed, perr := toInt64(value)
		if perr != nil {
			return fieldError(key, name, value, "a whole number")
		}
		err = doc.AddInt64Field(name, parsed)
	case zvec.DataTypeUint32:
		parsed, perr := toInt64(value)
		if perr != nil || parsed < 0 || parsed > math.MaxUint32 {
			return fieldError(key, name, value, "a whole number from 0 to 4294967295")
		}
		err = doc.AddUint32Field(name, uint32(parsed))
	case zvec.DataTypeUint64:
		parsed, perr := toUint64(value)
		if perr != nil {
			return fieldError(key, name, value, "a whole number of zero or more")
		}
		err = doc.AddUint64Field(name, parsed)
	case zvec.DataTypeFloat:
		parsed, perr := toFloat(value)
		if perr != nil {
			return fieldError(key, name, value, "a number")
		}
		err = doc.AddFloatField(name, float32(parsed))
	case zvec.DataTypeDouble:
		parsed, perr := toFloat(value)
		if perr != nil {
			return fieldError(key, name, value, "a number")
		}
		err = doc.AddDoubleField(name, parsed)
	case zvec.DataTypeVectorFP32:
		vector, verr := Vector(value, name)
		if verr != nil {
			return fmt.Errorf("document %q: %w", key, verr)
		}
		err = doc.AddVectorFP32Field(name, vector)
	case zvec.DataTypeBinary:
		data, berr := toBytes(value)
		if berr != nil {
			return fieldError(key, name, value, "base64 text or an array of byte values")
		}
		err = doc.AddBinaryField(name, data)
	default:
		return fmt.Errorf("document %q: the field %q has type %s, which this plugin cannot write yet", key, name, declared.dataType)
	}
	if err != nil {
		return fmt.Errorf("cannot set %q on document %q: %w", name, key, err)
	}
	return nil
}

// fromDoc reads an engine document back into a Document. Only the fields the
// document actually carries are read — a search that asked for three output
// fields returns three.
func (h *Handle) fromDoc(doc *zvec.Doc, includeDocID bool) Document {
	out := Document{Key: doc.GetPK(), Score: doc.GetScore()}
	if includeDocID {
		out.DocID = doc.GetDocID()
	}

	names, err := doc.GetFieldNames()
	if err != nil || len(names) == 0 {
		return out
	}
	out.Fields = make(map[string]any, len(names))
	for _, name := range names {
		if doc.IsFieldNull(name) {
			out.Fields[name] = nil
			continue
		}
		value, ok := h.readField(doc, name)
		if ok {
			out.Fields[name] = value
		}
	}
	return out
}

// readField reads one field by its declared type. A field the schema does not
// know (which should not happen for a document the engine produced) is read as
// a string, so an unexpected type degrades to something readable rather than
// disappearing.
func (h *Handle) readField(doc *zvec.Doc, name string) (any, bool) {
	declared := h.fieldType(name)
	var (
		value any
		err   error
	)
	switch declared.dataType {
	case zvec.DataTypeBool:
		value, err = doc.GetBoolField(name)
	case zvec.DataTypeInt32:
		value, err = doc.GetInt32Field(name)
	case zvec.DataTypeInt64:
		value, err = doc.GetInt64Field(name)
	case zvec.DataTypeUint32:
		value, err = doc.GetUint32Field(name)
	case zvec.DataTypeUint64:
		value, err = doc.GetUint64Field(name)
	case zvec.DataTypeFloat:
		value, err = doc.GetFloatField(name)
	case zvec.DataTypeDouble:
		value, err = doc.GetDoubleField(name)
	case zvec.DataTypeVectorFP32:
		value, err = doc.GetVectorFP32Field(name)
	default:
		value, err = doc.GetStringField(name)
	}
	if err != nil {
		return nil, false
	}
	return value, true
}

// Hoist moves the collection's vector and text fields out of Fields and onto
// the Document's own Vector and Text, so a result reads the same way an upsert
// is written. Fields the caller did not name are left where they are.
func (d *Document) Hoist(vectorField, textField string) {
	if len(d.Fields) == 0 {
		return
	}
	if vectorField != "" {
		if raw, ok := d.Fields[vectorField]; ok {
			if vector, ok := raw.([]float32); ok {
				d.Vector = vector
				delete(d.Fields, vectorField)
			}
		}
	}
	if textField != "" {
		if raw, ok := d.Fields[textField]; ok {
			if text, ok := raw.(string); ok {
				d.Text = text
				delete(d.Fields, textField)
			}
		}
	}
	if len(d.Fields) == 0 {
		d.Fields = nil
	}
}

func unknownField(name, what string) error {
	return fmt.Errorf("the collection has no %s called %q: check the name against the collection's schema (the Collection info action lists it)", what, name)
}

func fieldError(key, name string, value any, want string) error {
	return fmt.Errorf("document %q: the field %q is %v, which is not %s", key, name, value, want)
}

func sortedFieldNames(fields map[string]any) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func toBool(value any) (bool, error) {
	switch typed := value.(type) {
	case bool:
		return typed, nil
	case float64:
		return typed != 0, nil
	case string:
		return strconv.ParseBool(strings.TrimSpace(typed))
	default:
		return false, fmt.Errorf("not a boolean")
	}
}

func toInt64(value any) (int64, error) {
	parsed, err := toInt(value)
	return int64(parsed), err
}

func toUint64(value any) (uint64, error) {
	switch typed := value.(type) {
	case string:
		return strconv.ParseUint(strings.TrimSpace(typed), 10, 64)
	default:
		parsed, err := toInt64(value)
		if err != nil {
			return 0, err
		}
		if parsed < 0 {
			return 0, fmt.Errorf("negative")
		}
		return uint64(parsed), nil
	}
}

// toBytes reads a binary field, which a JSON document can only express as
// base64 text or as an array of byte values.
func toBytes(value any) ([]byte, error) {
	switch typed := value.(type) {
	case []byte:
		return typed, nil
	case string:
		return decodeBase64(typed)
	case []any:
		data := make([]byte, len(typed))
		for i, item := range typed {
			parsed, err := toInt(item)
			if err != nil || parsed < 0 || parsed > 255 {
				return nil, fmt.Errorf("not a byte value")
			}
			data[i] = byte(parsed)
		}
		return data, nil
	default:
		return nil, fmt.Errorf("not binary")
	}
}

// decodeBase64 accepts either of the two base64 alphabets, and either padded or
// not, because a value pasted from another system may be any of the four.
func decodeBase64(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if data, err := encoding.DecodeString(text); err == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("not base64")
}
