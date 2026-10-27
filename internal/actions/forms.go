package actions

import (
	"github.com/Inflowenger/go-plugin-sdk/formkit"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/Inflowenger/zvec-plugin/internal/zvecdb"
)

// Every form this plugin serves is declared here with the SDK's formkit
// builder, which generates the JSON Schema and the JSON Forms UI schema from
// one statement per field. A malformed form panics at start-up, where it is a
// compile-time-shaped mistake rather than a dialog that will not open.
//
// Each form is kept in step with the input struct that decodes it, in the
// action file beside this one: the field names here are the JSON tags there.

// ---------------------------------------------------------------- settings --

const descDataDir = `Directory on this host holding the collections — each collection is a subdirectory of it, e.g. /var/lib/inflow/zvec. It is created if it does not exist. zvec is an embedded database: there is no server to point at, and the data lives wherever this plugin process runs.`

const descReadOnly = `Open every collection read-only. Writes are then refused by the engine itself, which makes this a real guarantee rather than a convention — useful for a profile that several flows search but none should modify.`

const descMmap = `Map segment files instead of reading them into the process. Lowers resident memory on a large collection at the cost of page-cache pressure. Leave it off unless memory is tight.`

const descEmbedModel = `OpenAI: text-embedding-3-small (1536) or -3-large (3072). Gemini: text-embedding-004 (768). Cohere: embed-english-v3.0 (1024). Custom: whatever your endpoint serves. Blank uses the provider's default. Press ↻ to embed a probe and read back the dimension — that number is the collection's Vector size.`

var settingsForm = formkit.New("zvec storage").
	Describe("Where this node keeps its vectors, and optionally how to turn text into them. Stored by the platform as a reusable settings profile and shipped with every call as body.settings — the plugin keeps nothing.").
	SubmitTo("zvec.settings.check").
	Group("Storage",
		formkit.Text("dataDir", "Data directory").
			Required().
			Default("./zvec-data").
			Describe(descDataDir).
			Lookup("zvec.meta.ping", "Check"),
		formkit.Bool("readOnly", "Read-only").Default(false).Describe(descReadOnly),
		formkit.Bool("enableMmap", "Memory-map segments").Default(false).Describe(descMmap),
	).
	Group("Embeddings (optional)",
		formkit.Choice("embeddingProvider", "Provider",
			formkit.Option{Value: "none", Label: "None — I supply vectors myself"},
			formkit.Option{Value: "openai", Label: "OpenAI"},
			formkit.Option{Value: "gemini", Label: "Google Gemini"},
			formkit.Option{Value: "cohere", Label: "Cohere"},
			formkit.Option{Value: "custom", Label: "Custom (OpenAI-compatible: Ollama, LM Studio, vLLM…)"},
		).
			Default("none").
			Describe("Pick a provider and the Upsert, Search and Hybrid search actions accept plain text, embedding it on the way. With None they require a vector."),
		formkit.Text("embeddingModel", "Model").
			HideWhen("embeddingProvider", "none").
			Describe(descEmbedModel).
			Lookup("zvec.meta.embed", "Test embedding"),
		formkit.Secret("embeddingApiKey", "API key").
			HideWhen("embeddingProvider", "none").
			Describe("The embedding provider's key. Optional for a local, unsecured custom endpoint."),
		formkit.Text("embeddingBaseUrl", "Base URL").
			HideWhen("embeddingProvider", "none").
			Describe("Required for Custom, e.g. http://localhost:11434 for Ollama. An optional override for the hosted providers."),
	).
	Build()

// ------------------------------------------------------------ shared parts --

const descCollection = `Name of the collection to act on. Letters, digits, dot, dash and underscore — it becomes a directory under the profile's data directory. Accepts {{$.path}} tokens. Press ↻ to pick from the collections already there.`

const descFilter = `Filter expression, SQL-like and evaluated before the search so it narrows what is searched rather than what is returned, e.g. category = 'bikes' AND qty > 2. Equality is a single = (not ==). AND, OR, NOT, IN, BETWEEN and LIKE are available. A field can only be filtered on if it was created as filterable. Accepts {{$.path}} tokens.`

const descFilters = `Each row narrows the search to documents whose field equals the value, e.g. category → bikes. They are combined with the Filter box above (everything must match). A value that reads as a number or boolean matches one — quote it ("42") to match text instead. Both halves accept {{$.path}} tokens.`

const descOutputFields = `Fields to return with each result. Leave empty for all of them. Naming just the few a downstream node reads keeps the committed result small.`

const descSearchParams = `Optional JSON of the index's search-time knobs, applied to whichever index the field has. HNSW: {"ef": 100}. IVF and IVF-RaBitQ: {"nprobe": 16}. DiskANN: {"listSize": 100}. Also "radius", "isLinear", "isUsingRefiner", "scaleFactor" where the index supports them. Leave empty for the engine's own defaults.`

// collectionField is the box nearly every action starts with. pickForm names
// the action whose form the ↻ button rebuilds with this field turned into a
// drop-down of the collections that exist; leave it empty where a live list
// makes no sense, as on Create, which names one that does not exist yet.
func collectionField(pickForm string) *formkit.Field {
	field := formkit.Text("collection", "Collection").Required().Describe(descCollection)
	if pickForm != "" {
		field.Lookup("zvec.meta.collections", "List").Picks(pickForm)
	}
	return field
}

// keyValueRows is an add/remove list of key/value rows — the renderer draws a
// "+"/"-" row per entry and the handler receives [{key, value}, …]. It is how a
// form collects filters and field values without anyone hand-writing JSON.
func keyValueRows(name, title string) *formkit.Field {
	return formkit.Custom(name, title, map[string]any{
		"type": "array",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key":   map[string]any{"type": "string", "title": "Field"},
				"value": map[string]any{"type": "string", "title": "Value"},
			},
			"required": []any{"key"},
		},
	})
}

func filterField() *formkit.Field {
	return formkit.TextArea("filter", "Filter").Describe(descFilter)
}

func filterRows() *formkit.Field {
	return keyValueRows("filters", "Field filters").Describe(descFilters)
}

func outputFieldsField() *formkit.Field {
	return formkit.List("outputFields", "Return fields").Describe(descOutputFields)
}

func includeVectorField() *formkit.Field {
	return formkit.Bool("includeVector", "Include vectors").
		Default(false).
		Describe("Return the stored vectors with each result. They are large and rarely read downstream.")
}

func topKField() *formkit.Field {
	return formkit.Integer("topK", "Results").Default(10).Min(1).
		Describe("How many documents to return, best first.")
}

// ---------------------------------------------------------- collections --

const descVectorField = `Name of the field holding the vectors, e.g. embedding. Leave empty for a collection that only does full-text search.`

const descDim = `How many numbers each vector has. It must match the embedding model that will fill it — press ↻ on the settings dialog's Model box to read the number off your provider. It cannot be changed afterwards.`

const descIndexParams = `Optional JSON of the index's build-time knobs. HNSW: {"m": 16, "efConstruction": 200} — a larger m is a denser graph: better recall, more memory. IVF: {"nlist": 128, "nIters": 10, "useSoar": false}. IVF-RaBitQ: {"nlist": 128, "totalBits": 1, "sampleCount": 0}. DiskANN: {"maxDegree": 64, "listSize": 100, "pqChunkNum": 0}. Leave empty for the defaults shown.`

const descTextField = `Name of the field holding the text to search, e.g. content. It gets a full-text index, which is what the Full-text search and the text half of Hybrid search match against. Leave empty for a vector-only collection.`

const descTokenizerParams = `Optional JSON of tokenizer and filter options. standard: {"max_token_length": 40}. ngram: {"ngram_min": 2, "ngram_max": 3, "token_chars": ["letter","digit"]}. jieba: {"cut_mode": "search"}. With the stemmer filter: {"stemmer_lang": "english"}.`

const descFields = `The scalar fields each document carries beside its vector and text — what you later filter on and read back. Tick Filterable to build an index on the field, which is what makes it usable in a filter expression; tick Required to refuse a document that leaves it out.`

// fieldRows is the add/remove table of scalar fields on the Create form. Its
// shape is spelt out rather than built from formkit fields because it is an
// array of objects, which a form models as one property.
func fieldRows() *formkit.Field {
	types := make([]any, 0, len(zvecdb.ScalarTypeNames()))
	for _, name := range zvecdb.ScalarTypeNames() {
		types = append(types, name)
	}
	return formkit.Custom("fields", "Fields", map[string]any{
		"type": "array",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":       map[string]any{"type": "string", "title": "Name"},
				"type":       map[string]any{"type": "string", "title": "Type", "enum": types, "default": "string"},
				"filterable": map[string]any{"type": "boolean", "title": "Filterable", "default": true},
				"required":   map[string]any{"type": "boolean", "title": "Required", "default": false},
				"wildcard":   map[string]any{"type": "boolean", "title": "Allow LIKE", "default": false},
			},
			"required": []any{"name", "type"},
		},
	}).Describe(descFields)
}

var createForm = formkit.New("Create collection").
	Describe("Declare a collection's shape and create it. A zvec collection has a fixed schema: give it a vector field, a full-text field, or both, plus the scalar fields you want to filter on.").
	Add(collectionField("")).
	Group("Vector field",
		formkit.Text("vectorField", "Field name").Default("embedding").Describe(descVectorField),
		formkit.Integer("dim", "Vector size").Min(1).Describe(descDim),
		formkit.Enum("metric", "Distance", "cosine", "l2", "ip", "mipsl2").Default("cosine").
			Describe("How nearness is measured. Cosine is right for every embedding provider this plugin can call. It cannot be changed afterwards."),
		formkit.Enum("index", "Index", zvecdb.IndexNames...).Default("hnsw").
			Describe("hnsw is the general-purpose graph index. flat is exact and small-scale. ivf and ivf_rabitq trade recall for memory on large collections; diskann keeps the index on disk."),
		formkit.Enum("quantize", "Quantization", "none", "fp16", "int8", "int4").Default("none").
			Describe("Compress the stored vectors. Smaller and faster, at some cost in recall. Leave as none for full precision."),
		formkit.TextArea("indexParams", "Index params").Describe(descIndexParams),
	).
	Group("Text field (full-text search)",
		formkit.Text("textField", "Field name").Describe(descTextField),
		formkit.Enum("tokenizer", "Tokenizer", zvecdb.TokenizerNames...).Default("standard").
			Describe("standard splits ordinary prose. whitespace splits on spaces only. ngram matches substrings, for partial words and code. jieba segments Chinese."),
		formkit.Custom("tokenFilters", "Token filters", map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string", "enum": toAny(zvecdb.TokenFilterNames)},
		}).
			Default([]any{"lowercase"}).
			Describe("Applied to every token as it is indexed and searched. lowercase makes matching case-insensitive, ascii_folding makes café match cafe, stemmer makes jumping match jumps."),
		formkit.TextArea("tokenizerParams", "Tokenizer params").Describe(descTokenizerParams),
	).
	Group("Fields", fieldRows()).
	Group("Advanced",
		formkit.Integer("maxDocsPerSegment", "Max documents per segment").Min(1).
			Describe("Optional. Caps how many documents one storage segment holds. Leave empty for the engine's default."),
		formkit.Bool("ifNotExists", "Only if missing").Default(true).
			Describe("Leave an existing collection alone instead of failing. Keep this on for a create that sits in a flow which runs more than once."),
	).
	Build()

var listForm = formkit.New("List collections").
	Describe("Names every collection in the profile's data directory. No input.").
	Build()

var infoForm = formkit.New("Collection info").
	Describe("Document count, index completeness and the fields the collection declares.").
	Add(collectionField("zvec.collection.info")).
	Build()

var optimizeForm = formkit.New("Optimize collection").
	Describe("Flush pending writes to disk and merge segments, finishing the indexes that writes left incomplete. Worth running after a bulk load — until then those documents are searched by brute force.").
	Add(
		collectionField("zvec.collection.optimize"),
		formkit.Bool("flush", "Flush to disk").Default(true),
		formkit.Bool("optimize", "Optimize").Default(true).
			Describe("The expensive half: it rebuilds indexes and merges segments, and on a large collection it takes a while."),
	).
	Build()

var dropForm = formkit.New("Drop collection").
	Describe("Delete a collection and every document in it. This cannot be undone.").
	Add(
		collectionField("zvec.collection.drop"),
		formkit.Bool("confirm", "Yes, delete it").Default(false).
			Describe("The action refuses to run until this is ticked. It deletes the collection's directory from disk."),
	).
	Build()

// ------------------------------------------------------------- documents --

const descUpsertText = `The document's text. It is embedded with the configured provider into the vector field, and stored in the collection's text field so a full-text search can match it and a result can carry it back. Accepts {{$.path}} tokens.`

const descUpsertVector = `An explicit vector instead of embedding Text, e.g. [0.1, 0.2, 0.3]. Accepts {{$.path}} tokens.`

const descUpsertValues = `The document's scalar fields, by the names the collection declared, e.g. category → bikes. A value that reads as a number or boolean is stored as one — quote it ("42") to force text. Both halves accept {{$.path}} tokens.`

const descDocuments = `A JSON array of documents, which replaces the single document above. Each is {"key": "…", "text": "…", "fields": {…}} — or carries "vector" instead of "text". A key is generated when left out, and every text in the batch is embedded in one call to the provider. Accepts {{$.path}} tokens.`

const descWriteMode = `upsert writes the document, replacing one that already has its key. insert refuses a key that exists. update changes only the fields given, leaving the rest of the document — including its vector — as it was.`

const descVectorFieldOverride = `Which field the vector goes in. Leave empty to use the one the collection was created with.`

const descTextFieldOverride = `Which field the text goes in. Leave empty to use the one the collection was created with.`

var upsertForm = formkit.New("Upsert documents").
	Describe("Write documents into a collection. Give one document in the boxes below, or paste a batch as JSON.").
	Add(collectionField("zvec.doc.upsert")).
	Group("Document",
		formkit.Text("key", "Key").
			Describe("The document's primary key — what Fetch and Delete address it by. A UUID is generated when left empty. Writing the same key again replaces that document. Accepts {{$.path}} tokens."),
		formkit.TextArea("text", "Text").Describe(descUpsertText),
		formkit.TextArea("vector", "Vector").Describe(descUpsertVector),
		keyValueRows("values", "Field values").Describe(descUpsertValues),
	).
	Group("Batch (advanced)",
		formkit.TextArea("documents", "Documents").Describe(descDocuments),
	).
	Group("Options",
		formkit.Enum("mode", "Mode", "upsert", "insert", "update").Default("upsert").Describe(descWriteMode),
		formkit.Bool("flush", "Flush to disk").Default(true).
			Describe("Make the write durable, and visible to a search, before the node finishes. Turn it off only when many upserts run in a row and an Optimize follows them."),
		formkit.Text("vectorField", "Vector field").Describe(descVectorFieldOverride),
		formkit.Text("textField", "Text field").Describe(descTextFieldOverride),
	).
	Build()

var searchForm = formkit.New("Vector search").
	Describe("Nearest-neighbour search: find the documents whose vectors are closest to the query, optionally narrowed by a filter.").
	Add(collectionField("zvec.doc.search")).
	Group("Query",
		formkit.TextArea("text", "Query text").
			Describe("Embedded with the configured provider and searched. Leave empty to search by a raw vector instead. Accepts {{$.path}} tokens."),
		formkit.TextArea("vector", "Query vector").
			Describe("An explicit vector, e.g. [0.1, 0.2, 0.3]. Used when Query text is empty. Accepts {{$.path}} tokens."),
		topKField(),
		filterField(),
		filterRows(),
	).
	Group("Options",
		outputFieldsField(),
		includeVectorField(),
		formkit.Text("vectorField", "Vector field").Describe(descVectorFieldOverride).
			Lookup("zvec.meta.fields", "List").Picks("zvec.doc.search"),
		formkit.TextArea("searchParams", "Search params").Describe(descSearchParams),
	).
	Build()

const descMatch = `Words to look for, scored by relevance — the ordinary way to search text, e.g. waterproof jacket. Accepts {{$.path}} tokens.`

const descFTSQuery = `A boolean expression instead of Match text: fox AND forest, fox NOT dog, fox OR hound. Accepts {{$.path}} tokens.`

const descOperator = `How adjacent bare words in the Query expression are joined. Leave empty for the engine's own default.`

var ftsForm = formkit.New("Full-text search").
	Describe("Search the collection's text field by words rather than by vector — exact terms, names and codes an embedding tends to blur.").
	Add(collectionField("zvec.doc.fts")).
	Group("Query",
		formkit.TextArea("match", "Match text").Describe(descMatch),
		formkit.TextArea("query", "Query expression").Describe(descFTSQuery),
		formkit.Enum("operator", "Default operator", "", "OR", "AND").Describe(descOperator),
		topKField(),
		filterField(),
		filterRows(),
	).
	Group("Options",
		outputFieldsField(),
		includeVectorField(),
		formkit.Text("textField", "Text field").Describe(descTextFieldOverride).
			Lookup("zvec.meta.fields", "List").Picks("zvec.doc.fts"),
	).
	Build()

const descHybridText = `The query, used by both halves: embedded for the vector half, and matched as words by the text half. Fill in just this box for the common case. Accepts {{$.path}} tokens.`

const descRerank = `How the two rankings are fused. rrf merges them by where each document placed in each list, which needs no comparable scores and is the right default. weighted scales the raw scores instead, which is sharper when you know how much each half should count.`

const descCandidates = `How many documents this half contributes to the fusion. It should comfortably exceed Results — fusion can only reorder what it is given, so a half offering only as many candidates as you want back can contribute nothing the other half did not already find. Empty uses five times Results, at least 50.`

var hybridForm = formkit.New("Hybrid search").
	Describe("Search by meaning and by words at once, and fuse the two rankings. It finds what a vector search alone misses — an exact code, a rare name — without losing what only a vector search finds.").
	Add(collectionField("zvec.doc.hybrid")).
	Group("Query",
		formkit.TextArea("text", "Query text").Describe(descHybridText),
		topKField(),
		filterField(),
		filterRows(),
	).
	Group("Fusion",
		formkit.Enum("rerank", "Rerank", "rrf", "weighted").Default("rrf").Describe(descRerank),
		formkit.Integer("rrfK", "RRF rank constant").Default(60).Min(1).
			HideWhen("rerank", "weighted").
			Describe("The larger it is, the flatter the weight given to the top of each list. 60 is the usual starting point."),
		formkit.Number("vectorWeight", "Vector weight").Default(0.5).
			ShowWhen("rerank", "weighted").
			Describe("How much the vector half counts, against the text weight below."),
		formkit.Number("textWeight", "Text weight").Default(0.5).
			ShowWhen("rerank", "weighted").
			Describe("How much the full-text half counts."),
	).
	Group("Vector half",
		formkit.TextArea("vector", "Query vector").
			Describe("An explicit vector, used instead of embedding the query text. Accepts {{$.path}} tokens."),
		formkit.Text("vectorField", "Vector field").Describe(descVectorFieldOverride),
		formkit.Integer("vectorCandidates", "Candidates").Min(1).Describe(descCandidates),
		formkit.TextArea("searchParams", "Search params").Describe(descSearchParams),
	).
	Group("Text half",
		formkit.TextArea("match", "Match text").
			Describe("Overrides the query text for the full-text half only. Accepts {{$.path}} tokens."),
		formkit.TextArea("query", "Query expression").Describe(descFTSQuery),
		formkit.Enum("operator", "Default operator", "", "OR", "AND").Describe(descOperator),
		formkit.Text("textField", "Text field").Describe(descTextFieldOverride),
		formkit.Integer("textCandidates", "Candidates").Min(1).Describe(descCandidates),
	).
	Group("Options",
		outputFieldsField(),
		includeVectorField(),
	).
	Build()

var fetchForm = formkit.New("Fetch documents").
	Describe("Read documents by their keys. Keys that are not there are simply absent from the result, which makes this usable as an existence test.").
	Add(
		collectionField("zvec.doc.fetch"),
		formkit.List("keys", "Keys").Required().
			Describe("The document keys to read, one per row. Each accepts {{$.path}} tokens."),
		outputFieldsField(),
		includeVectorField(),
	).
	Build()

const descScan = `Walk the collection in storage order. The engine's cursor cannot be handed to a later run, so paging across runs means skipping what was already seen, which re-reads the earlier pages — fine for a few thousand documents, the wrong tool for a million. To narrow a large collection, filter on an indexed field with Vector search instead.`

var scanForm = formkit.New("Scan documents").
	Describe(descScan).
	Add(
		collectionField("zvec.doc.scan"),
		formkit.Integer("limit", "Limit").Default(50).Min(1).
			Describe("Most documents to return."),
		formkit.Integer("skip", "Skip").Default(0).Min(0).
			Describe("How many to pass over first. Set it to the running total of what previous runs returned — often a {{$.path}} token pointing at the last run's result."),
		outputFieldsField(),
		includeVectorField(),
	).
	Build()

var deleteForm = formkit.New("Delete documents").
	Describe("Delete documents by their keys, or by a filter. Give exactly one.").
	Add(
		collectionField("zvec.doc.delete"),
		formkit.List("keys", "Keys").
			Describe("The document keys to delete, one per row. Each accepts {{$.path}} tokens."),
		filterField(),
		filterRows(),
		formkit.Bool("flush", "Flush to disk").Default(true),
	).
	Build()

// pickableForms maps an action method to the form its ↻ buttons rebuild, so a
// lookup can turn a box into a drop-down of what it found. The method names
// must match the Picks() calls above; a form missing here still works, its
// lookup just lists the candidates as text instead.
var pickableForms = map[string]sdkv1.FormBuilder{
	"zvec.collection.info":     infoForm,
	"zvec.collection.optimize": optimizeForm,
	"zvec.collection.drop":     dropForm,
	"zvec.doc.upsert":          upsertForm,
	"zvec.doc.search":          searchForm,
	"zvec.doc.fts":             ftsForm,
	"zvec.doc.hybrid":          hybridForm,
	"zvec.doc.fetch":           fetchForm,
	"zvec.doc.scan":            scanForm,
	"zvec.doc.delete":          deleteForm,
}

func toAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}
