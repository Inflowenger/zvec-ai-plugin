# zvec

Search by meaning, by words, or by both at once — from a workflow, with no
database server to run.

[zvec](https://github.com/alibaba/zvec) is an **embedded** vector database: it
is a library, not a service. This node keeps its collections in a directory on
whichever host the plugin process runs on. There is nothing to deploy beside it,
nothing to keep healthy, and no network hop between a flow and its vectors.

## Setup

Open the node's **settings**.

- **Data directory** — where the collections live, e.g. `/var/lib/inflow/zvec`.
  It is created if it does not exist, and each collection becomes a subdirectory
  of it. Press **Check** to confirm it is usable and see what is already there.
- **Read-only** — open every collection read-only. The engine then refuses
  writes itself, which makes a search-only profile a real guarantee.
- **Memory-map segments** — lowers resident memory on a large collection.

One plugin process per data directory: a collection is held under an exclusive
lock while it is open, so two processes pointed at one directory will fight over
it.

### Embeddings (optional)

zvec stores whatever numbers you give it. To search *content*, something has to
turn text into vectors — configure a provider and **Upsert**, **Vector search**
and **Hybrid search** will do it for you:

- **Provider** — OpenAI, Google Gemini, Cohere, or Custom (any OpenAI-compatible
  endpoint: Ollama, LM Studio, vLLM). Leave it **None** to supply vectors
  yourself.
- **Model** — e.g. `text-embedding-3-small` (1536), `text-embedding-004` (768),
  `embed-english-v3.0` (1024). Blank uses the provider's default.
- **API key**, **Base URL** — the provider's own; Base URL is required for
  Custom.

Press **Test embedding** to embed a probe and read back the **dimension**. That
number is the **Vector size** a collection must be created with — a collection
and the model that fills it have to agree, and it cannot be changed later.

## Actions

| Action | What it does |
|--------|--------------|
| **Create collection** | Declare a collection's shape and create it. |
| **List collections** | Name every collection in the data directory. |
| **Collection info** | Document count, index completeness, and the fields. |
| **Optimize collection** | Flush and merge segments; finish the indexes. |
| **Drop collection** | Delete a collection and its data. |
| **Upsert documents** | Write documents, embedding their text. |
| **Vector search** | Nearest neighbours by meaning. |
| **Full-text search** | Matching by word. |
| **Hybrid search** | Both at once, with the rankings fused. |
| **Fetch documents** | Read documents by key. |
| **Scan documents** | Walk the collection a page at a time. |
| **Delete documents** | Delete by key, or by filter. |

Every **Collection** box has a **List** button (↻) — press it to pick from the
collections that exist instead of typing a name.

## Creating a collection

Unlike a document store, a zvec collection has a **fixed schema**, declared once:

- A **vector field** — its name, its size, the distance metric, and the index.
  `hnsw` is the general-purpose choice; `flat` is exact and best for small
  collections.
- A **text field** — the field to full-text index, with a tokenizer
  (`standard` for prose, `ngram` for substrings and codes, `jieba` for Chinese)
  and token filters (`lowercase`, `ascii_folding`, `stemmer`).
- **Fields** — the scalars each document carries. Tick **Filterable** to index
  one, which is what makes it usable in a filter.

Give it a vector field, a text field, or both. Both is what makes **Hybrid
search** available, and is the usual choice.

## Hybrid search

A vector search finds what a query *means*. A full-text search finds what it
*says*. Each misses what the other catches: an embedding blurs an exact part
number, and a word match knows nothing about synonyms.

**Hybrid search** runs both and fuses the rankings. Fill in just **Query
text** — the vector half embeds it, the text half matches it as words:

> **Query text:** `waterproof jacket for hiking`

Two ways to fuse:

- **rrf** (the default) merges the two lists by *where* each document placed in
  each, so the two halves' scores never have to be comparable. Start here.
- **weighted** scales the raw scores by **Vector weight** and **Text weight**.
  Sharper, once you know how much each half should count.

Each half contributes **Candidates** documents to the fusion — by default five
times **Results**, at least 50. That matters: fusion can only reorder what it is
given, so a half offering only as many candidates as you want back contributes
nothing the other half did not already find.

If the collection has only one of the two halves, Hybrid search runs as that
half's own search rather than failing.

## Filtering

Filters are **SQL-like expressions**, and they narrow what is *searched*, not
what is returned — so a filtered search still returns a full set of results.

```
category = 'bikes' AND qty > 2
```

Equality is a single `=`, not `==`. `AND`, `OR`, `NOT`, `IN`, `BETWEEN` and
`LIKE` are available. A field can only be filtered on if it was created
**filterable**.

The **Field filters** rows are the easy path for the common case: each row is
one `field = value`, and they are combined with the Filter box so everything has
to match. A value that reads as a number or boolean matches one — quote it
(`"42"`) to match text instead.

## Documents

A document is a **key**, optional **text**, a **vector**, and the collection's
scalar **fields**:

- **Key** — what Fetch and Delete address it by. A UUID is generated when left
  blank. Writing the same key again replaces that document.
- **Text** — embedded into the vector field, *and* stored in the text field so
  full-text search can match it and results carry it back.
- **Field values** — key/value rows, by the names the collection declared.

For a batch, paste a JSON array into **Documents** instead, which overrides the
single-document boxes. Every text in the batch is embedded in one call:

```json
[
  { "key": "b1", "text": "A red mountain bike", "fields": { "category": "bikes", "qty": 4 } },
  { "key": "j7", "text": "A waterproof rain jacket", "fields": { "category": "gear", "qty": 9 } }
]
```

That is the same shape a search returns, so a search result can be fed straight
back into an upsert.

**Mode** chooses how the write lands: `upsert` replaces, `insert` refuses an
existing key, and `update` changes only the fields given — which is how one
field is changed without resending the vector.

## JsonPath tokens

**Every** text and JSON field on every action may contain `{{$.path}}` tokens,
resolved from the flow's context before the action runs — the collection name, a
query, both halves of a key/value row, the keys to fetch, and the contents of a
JSON box. Resolution reaches any depth, so a token works wherever it sits:

- Query text `{{$.trigger.question}}` → searches what the trigger asked.
- Field filter value `{{$.tenant}}` → narrows to the current tenant.
- Scan **Skip** `{{$.scanned.nextSkip}}` → carries on from the previous run.

A token the context cannot supply is left in place verbatim, so nothing is
silently dropped. (The settings profile is not part of a run and is not
resolved.)

## After a bulk load

Writes go into new segments, and until those are merged their documents are
searched by brute force. After loading a lot of documents, run **Optimize
collection** once: it merges the segments and finishes the indexes. **Collection
info** shows each index's completeness, which is how you tell whether it is
needed.

## Check the storage

The button below checks the saved settings profile's data directory and lists
what it holds.

```inflow-meta
zvec.meta.ping
```
