package actions

import (
	"reflect"
	"testing"
)

// resolverWith builds a resolver whose paths are already answered, so the
// traversal can be tested without a live flow — resolve() reads the cache
// before it ever asks the runtime.
func resolverWith(values map[string]string) *varResolver {
	return &varResolver{cache: values}
}

func TestResolveSubstitutesTokens(t *testing.T) {
	vr := resolverWith(map[string]string{"$.tenant": "acme", "$.n": "42"})
	cases := map[string]string{
		"{{$.tenant}}":            "acme",
		"{{ $.tenant }}":          "acme",
		"tenant = '{{$.tenant}}'": "tenant = 'acme'",
		"{{$.tenant}}/{{$.n}}":    "acme/42",
		"nothing to do":           "nothing to do",
		"{{$.unknown}}":           "{{$.unknown}}", // left in place, never dropped
	}
	for in, want := range cases {
		if got := vr.resolve(in); got != want {
			t.Errorf("resolve(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every text-bearing field of an action has to be reachable, at any depth —
// that is what makes tokens work uniformly rather than only where someone
// remembered to call the resolver.
func TestResolveReachesEveryStringInAnInput(t *testing.T) {
	vr := resolverWith(map[string]string{
		"$.coll":   "docs",
		"$.q":      "red bicycle",
		"$.tenant": "acme",
		"$.key":    "k1",
		"$.field":  "category",
	})

	in := hybridInput{
		target:       target{Collection: "{{$.coll}}"},
		Text:         "{{$.q}}",
		Filter:       "tenant = '{{$.tenant}}'",
		Filters:      []valueRow{{Key: "{{$.field}}", Value: "{{$.tenant}}"}},
		OutputFields: []string{"{{$.field}}", "qty"},
		Vector:       `[1, 2]`,
		SearchParams: map[string]any{"note": "{{$.key}}", "ef": float64(64)},
	}
	resolveValue(reflect.ValueOf(&in).Elem(), vr)

	if in.Collection != "docs" {
		t.Errorf("embedded struct field not resolved: %q", in.Collection)
	}
	if in.Text != "red bicycle" {
		t.Errorf("Text = %q", in.Text)
	}
	if in.Filter != "tenant = 'acme'" {
		t.Errorf("Filter = %q", in.Filter)
	}
	if in.Filters[0].Key != "category" || in.Filters[0].Value != "acme" {
		t.Errorf("both halves of a row should resolve: %+v", in.Filters[0])
	}
	if in.OutputFields[0] != "category" || in.OutputFields[1] != "qty" {
		t.Errorf("list entries not resolved: %v", in.OutputFields)
	}

	// An `any` field holding a decoded map is rebuilt rather than skipped,
	// and its non-string values are left alone.
	params, ok := in.SearchParams.(map[string]any)
	if !ok {
		t.Fatalf("SearchParams = %#v", in.SearchParams)
	}
	if params["note"] != "k1" {
		t.Errorf("a string inside a JSON field was not resolved: %#v", params["note"])
	}
	if params["ef"] != float64(64) {
		t.Errorf("a number inside a JSON field was altered: %#v", params["ef"])
	}
}

func TestResolveHandlesAJSONFieldThatArrivedAsText(t *testing.T) {
	vr := resolverWith(map[string]string{"$.vec": "[0.1, 0.2]"})
	in := searchInput{Vector: "{{$.vec}}"}
	resolveValue(reflect.ValueOf(&in).Elem(), vr)
	if in.Vector != "[0.1, 0.2]" {
		t.Fatalf("Vector = %#v", in.Vector)
	}
}

func TestResolveLeavesNilAndEmptyAlone(t *testing.T) {
	vr := resolverWith(nil)
	in := upsertInput{}
	resolveValue(reflect.ValueOf(&in).Elem(), vr)
	if in.Documents != nil || in.Vector != nil || in.Values != nil {
		t.Fatalf("empty input was altered: %+v", in)
	}
}

func TestResolveAnyDescendsNestedStructures(t *testing.T) {
	vr := resolverWith(map[string]string{"$.a": "A", "$.b": "B"})
	got := resolveAny(map[string]any{
		"plain": "x",
		"token": "{{$.a}}",
		"list":  []any{"{{$.b}}", float64(1), nil},
		"deep":  map[string]any{"{{$.a}}": "{{$.b}}"},
	}, vr)

	out, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("got %#v", got)
	}
	if out["token"] != "A" {
		t.Errorf("token = %#v", out["token"])
	}
	list, _ := out["list"].([]any)
	if len(list) != 3 || list[0] != "B" || list[1] != float64(1) || list[2] != nil {
		t.Errorf("list = %#v", out["list"])
	}
	deep, _ := out["deep"].(map[string]any)
	if deep["A"] != "B" {
		t.Errorf("a token in a key should resolve too: %#v", deep)
	}
}
