package actions

import (
	"strings"
	"testing"

	"github.com/Inflowenger/zvec-ai-plugin/internal/zvecdb"
)

func TestBuildFilterCombinesRowsWithTheExpression(t *testing.T) {
	cases := []struct {
		name       string
		expression string
		rows       []valueRow
		want       string
	}{
		{name: "expression only", expression: "qty > 2", want: "qty > 2"},
		{name: "rows only", rows: []valueRow{{Key: "category", Value: "bikes"}}, want: "category = 'bikes'"},
		{
			name:       "both",
			expression: "qty > 2",
			rows:       []valueRow{{Key: "category", Value: "bikes"}},
			want:       "(qty > 2) AND category = 'bikes'",
		},
		{
			name: "several rows all have to match",
			rows: []valueRow{{Key: "a", Value: "1"}, {Key: "b", Value: "x"}},
			want: "a = 1 AND b = 'x'",
		},
		{
			name: "blank rows the user left behind are ignored",
			rows: []valueRow{{Key: "", Value: ""}, {Key: "a", Value: "1"}},
			want: "a = 1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildFilter(tc.expression, tc.rows)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("buildFilter = %q, want %q", got, tc.want)
			}
		})
	}
}

// An expression containing OR has to be parenthesised, or a row meant to
// narrow the search would instead be an alternative to half of it.
func TestBuildFilterParenthesisesAnExpressionItAddsTo(t *testing.T) {
	got, err := buildFilter("a = 1 OR b = 2", []valueRow{{Key: "tenant", Value: "acme"}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "(a = 1 OR b = 2) AND tenant = 'acme'" {
		t.Fatalf("buildFilter = %q", got)
	}
}

func TestBuildFilterRefusesAValueWithNoField(t *testing.T) {
	if _, err := buildFilter("", []valueRow{{Value: "orphan"}}); err == nil {
		t.Fatal("a row with a value and no field name was accepted")
	}
}

func TestLiteralReadsTheTypeAValueDenotes(t *testing.T) {
	cases := map[string]string{
		"bikes":  `'bikes'`,
		"42":     `42`,
		"4.5":    `4.5`,
		"-7":     `-7`,
		"true":   `true`,
		"false":  `false`,
		`"42"`:   `'42'`,   // quoted to stay text
		`"true"`: `'true'`, // ditto
		"":       `''`,
	}
	for in, want := range cases {
		got, err := literal(in)
		if err != nil {
			t.Errorf("literal(%q) failed: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("literal(%q) = %q, want %q", in, got, want)
		}
	}
}

// A value carrying a quote would otherwise close the literal early and turn
// the rest of the value into filter syntax. The engine escapes with a
// backslash, not by doubling the quote as SQL does.
func TestLiteralEscapesQuotes(t *testing.T) {
	got, err := literal("O'Brien")
	if err != nil {
		t.Fatal(err)
	}
	if got != `'O\'Brien'` {
		t.Fatalf("literal = %q, want 'O\\'Brien'", got)
	}

	got, err = literal("x' OR 1=1 --")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
		t.Fatalf("literal = %q is not a single closed literal", got)
	}
	// Every quote from the value is escaped, so none of it can be read as
	// syntax: the only unescaped quotes are the two delimiters.
	if unescapedQuotes(got) != 2 {
		t.Fatalf("literal = %q carries an unescaped quote from the value", got)
	}
}

// unescapedQuotes counts the quotes that are not preceded by a backslash.
func unescapedQuotes(text string) int {
	count := 0
	for i := 0; i < len(text); i++ {
		if text[i] != '\'' {
			continue
		}
		if i > 0 && text[i-1] == '\\' {
			continue
		}
		count++
	}
	return count
}

// A value ending in a backslash would escape the literal's own closing quote,
// and the language has no way to write one. It is refused rather than emitted
// as a filter that parses into something else.
func TestLiteralRefusesAValueItCannotExpress(t *testing.T) {
	if _, err := literal(`ends with\`); err == nil {
		t.Fatal("a value ending in a backslash was accepted")
	}
	if _, err := buildFilter("", []valueRow{{Key: "name", Value: `x\`}}); err == nil {
		t.Fatal("buildFilter accepted a value ending in a backslash")
	}
}

func TestTypedReadsFieldValues(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"bikes", "bikes"},
		{"42", float64(42)},
		{"true", true},
		{"false", false},
		{`"42"`, "42"},
	}
	for _, tc := range cases {
		if got := typed(tc.in); got != tc.want {
			t.Errorf("typed(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestSingleDocumentNeedsSomethingToWrite(t *testing.T) {
	if _, err := (upsertInput{}).documents(); err == nil {
		t.Fatal("an empty upsert form was accepted")
	}
}

func TestSingleDocumentGeneratesAKeyWhenNoneIsGiven(t *testing.T) {
	docs, err := upsertInput{Text: "a red bicycle"}.documents()
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Key == "" {
		t.Fatalf("documents = %+v, want one document with a generated key", docs)
	}
	if docs[0].Text != "a red bicycle" {
		t.Fatalf("text = %q", docs[0].Text)
	}
}

func TestBatchDocumentsAcceptBothShapes(t *testing.T) {
	// The wrapped shape, which is what a search returns...
	wrapped := `[{"key":"a","text":"hello","fields":{"category":"greeting"}}]`
	// ...and the flat one, which is what someone types by hand.
	flat := `[{"key":"a","text":"hello","category":"greeting"}]`

	for _, body := range []string{wrapped, flat} {
		docs, err := upsertInput{Documents: body}.documents()
		if err != nil {
			t.Fatalf("documents(%s) failed: %v", body, err)
		}
		if len(docs) != 1 {
			t.Fatalf("got %d documents", len(docs))
		}
		if docs[0].Key != "a" || docs[0].Text != "hello" {
			t.Fatalf("document = %+v", docs[0])
		}
		if docs[0].Fields["category"] != "greeting" {
			t.Fatalf("fields = %+v, want category greeting", docs[0].Fields)
		}
	}
}

// A result from a search can be fed straight back into an upsert, so the keys
// a search produces must not end up as fields.
func TestBatchDocumentsDoNotTurnSearchMetadataIntoFields(t *testing.T) {
	docs, err := upsertInput{Documents: `[{"key":"a","text":"x","score":0.5,"docId":7,"vector":[1,2]}]`}.documents()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"score", "docId", "vector", "key", "text"} {
		if _, found := docs[0].Fields[name]; found {
			t.Errorf("%q was treated as a document field", name)
		}
	}
	if len(docs[0].Vector) != 2 {
		t.Fatalf("vector = %v, want it read as the document's vector", docs[0].Vector)
	}
}

func TestCreateInputBuildsASpec(t *testing.T) {
	in := createInput{
		target:       target{Collection: "docs"},
		VectorField:  "embedding",
		Dim:          768,
		Metric:       "cosine",
		Index:        "hnsw",
		IndexParams:  `{"m": 32}`,
		TextField:    "content",
		Tokenizer:    "standard",
		TokenFilters: []string{"lowercase"},
		Fields:       []fieldRow{{Name: "category", Type: "string"}},
	}
	spec, err := in.spec()
	if err != nil {
		t.Fatal(err)
	}
	if spec.VectorField() != "embedding" || spec.Vector.Dim != 768 {
		t.Fatalf("vector spec = %+v", spec.Vector)
	}
	if spec.TextField() != "content" {
		t.Fatalf("text spec = %+v", spec.Text)
	}
	if len(spec.Fields) != 1 || !spec.Fields[0].Filterable {
		t.Fatalf("fields = %+v, want category filterable by default", spec.Fields)
	}
}

func TestCreateInputRefusesAVectorFieldWithNoSize(t *testing.T) {
	_, err := createInput{target: target{Collection: "docs"}, VectorField: "embedding"}.spec()
	if err == nil {
		t.Fatal("a vector field with no dimension was accepted")
	}
	if !strings.Contains(err.Error(), "vector size") {
		t.Fatalf("err = %q, want it to name the missing vector size", err)
	}
}

func TestCreateInputRefusesACollectionWithNothingToSearch(t *testing.T) {
	if _, err := (createInput{target: target{Collection: "docs"}}).spec(); err == nil {
		t.Fatal("a collection with neither a vector nor a text field was accepted")
	}
}

func TestVectorFieldReadsBothTextAndParsedJSON(t *testing.T) {
	for _, raw := range []any{`[0.1, 0.2]`, []any{0.1, 0.2}} {
		vector, err := zvecdb.Vector(raw, "vector")
		if err != nil {
			t.Fatalf("Vector(%#v) failed: %v", raw, err)
		}
		if len(vector) != 2 || vector[0] != 0.1 {
			t.Fatalf("vector = %v", vector)
		}
	}
}
