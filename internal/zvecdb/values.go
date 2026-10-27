package zvecdb

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// paramBag reads an index's or a query's tuning knobs out of a free-form JSON
// object a user typed, keeping the first thing that was wrong with it.
//
// Collecting the error rather than returning it per read keeps the callers
// readable — a knob is a default and a one-line override, not a branch — while
// still refusing a value the user plainly meant and mistyped. An unknown key is
// deliberately ignored: the engine gains params between releases, and a form
// that rejected tomorrow's knob would be the thing standing in the way.
type paramBag struct {
	values map[string]any
	first  error
}

func params(values map[string]any) *paramBag { return &paramBag{values: values} }

func (p *paramBag) err() error { return p.first }

func (p *paramBag) fail(key string, value any, want string) {
	if p.first == nil {
		p.first = fmt.Errorf("%q is %v, which is not %s", key, value, want)
	}
}

// intOr reads a whole number, falling back to the default when the key is
// absent. JSON numbers arrive as float64, so a value like 16.0 is accepted and
// 16.5 is not.
func (p *paramBag) intOr(key string, fallback int) int {
	raw, ok := p.lookup(key)
	if !ok {
		return fallback
	}
	value, err := toInt(raw)
	if err != nil {
		p.fail(key, raw, "a whole number")
		return fallback
	}
	return value
}

func (p *paramBag) floatOr(key string, fallback float64) float64 {
	raw, ok := p.lookup(key)
	if !ok {
		return fallback
	}
	value, err := toFloat(raw)
	if err != nil {
		p.fail(key, raw, "a number")
		return fallback
	}
	return value
}

func (p *paramBag) boolOr(key string, fallback bool) bool {
	raw, ok := p.lookup(key)
	if !ok {
		return fallback
	}
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			p.fail(key, raw, "true or false")
			return fallback
		}
		return parsed
	default:
		p.fail(key, raw, "true or false")
		return fallback
	}
}

// lookup matches a knob leniently on case and separators, so "ef_construction"
// and "efConstruction" are one key — these are hand-typed into a JSON box.
func (p *paramBag) lookup(key string) (any, bool) {
	if value, ok := p.values[key]; ok {
		return value, value != nil
	}
	want := canonicalKey(key)
	for name, value := range p.values {
		if canonicalKey(name) == want {
			return value, value != nil
		}
	}
	return nil, false
}

func toInt(raw any) (int, error) {
	switch value := raw.(type) {
	case float64:
		if value != math.Trunc(value) {
			return 0, fmt.Errorf("not a whole number")
		}
		return int(value), nil
	case int:
		return value, nil
	case int64:
		return int(value), nil
	case json.Number:
		parsed, err := value.Int64()
		return int(parsed), err
	case string:
		return strconv.Atoi(strings.TrimSpace(value))
	case bool:
		return 0, fmt.Errorf("not a number")
	case nil:
		return 0, fmt.Errorf("not set")
	default:
		return 0, fmt.Errorf("not a number")
	}
}

func toFloat(raw any) (float64, error) {
	switch value := raw.(type) {
	case float64:
		return value, nil
	case float32:
		return float64(value), nil
	case int:
		return float64(value), nil
	case int64:
		return float64(value), nil
	case json.Number:
		return value.Float64()
	case string:
		return strconv.ParseFloat(strings.TrimSpace(value), 64)
	case nil:
		return 0, fmt.Errorf("not set")
	default:
		return 0, fmt.Errorf("not a number")
	}
}

// Vector reads a query or document vector out of whatever the form delivered:
// a JSON array of numbers, or the text of one.
func Vector(raw any, field string) ([]float32, error) {
	items, err := anyArray(raw, field)
	if err != nil {
		return nil, err
	}
	vector := make([]float32, len(items))
	for i, item := range items {
		value, err := toFloat(item)
		if err != nil {
			return nil, fmt.Errorf("%q[%d] is %v, which is not a number", field, i, item)
		}
		vector[i] = float32(value)
	}
	if len(vector) == 0 {
		return nil, fmt.Errorf("%q is an empty vector", field)
	}
	return vector, nil
}

// anyArray reads a JSON array that may arrive already parsed (the platform
// decoded it) or as the text the user typed into a multi-line box.
func anyArray(raw any, field string) ([]any, error) {
	decoded, err := decodeJSONish(raw, field)
	if err != nil {
		return nil, err
	}
	if decoded == nil {
		return nil, fmt.Errorf("%q is empty", field)
	}
	items, ok := decoded.([]any)
	if !ok {
		return nil, fmt.Errorf("%q must be a JSON array, e.g. [0.1, 0.2, 0.3]", field)
	}
	return items, nil
}

// anyObject is anyArray for a JSON object, returning nil when the field was
// left empty.
func anyObject(raw any, field string) (map[string]any, error) {
	decoded, err := decodeJSONish(raw, field)
	if err != nil || decoded == nil {
		return nil, err
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%q must be a JSON object, e.g. {\"m\": 16}", field)
	}
	return object, nil
}

// decodeJSONish turns a form field into a value: text is parsed as JSON, and
// anything the platform already decoded is passed through. An empty or
// whitespace-only field is nil, not an error — that is what "left blank" means
// on an optional field.
func decodeJSONish(raw any, field string) (any, error) {
	switch value := raw.(type) {
	case nil:
		return nil, nil
	case string:
		text := strings.TrimSpace(value)
		if text == "" {
			return nil, nil
		}
		var decoded any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil {
			return nil, fmt.Errorf("%q is not valid JSON: %s", field, err)
		}
		return decoded, nil
	default:
		return value, nil
	}
}

// Object reads an optional JSON object out of a form field, which may arrive
// as the text a user typed or as a value the platform already decoded. A blank
// field is nil rather than an error.
func Object(raw any, field string) (map[string]any, error) {
	return anyObject(raw, field)
}

// Array is Object for a JSON array.
func Array(raw any, field string) ([]any, error) {
	return anyArray(raw, field)
}

// IsBlank reports whether a form field was left empty.
func IsBlank(raw any) bool {
	decoded, err := decodeJSONish(raw, "")
	return err == nil && decoded == nil
}
