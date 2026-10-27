# zvec plugin (Inflowenger node)

An Inflowenger **Plugin node** for [zvec](https://github.com/alibaba/zvec), the
embedded vector database — including **hybrid search**, which runs a vector
query and a full-text query and fuses their rankings. Built on
[`go-plugin-sdk`](https://github.com/Inflowenger/go-plugin-sdk).

zvec is a **library, not a server**. There is no container to run beside this
plugin, no port, no credential and no network hop: a collection is a directory
on whichever host the plugin process runs on. That makes this the vector store
to reach for when a deployment should carry as little as possible — and the
reason it is *not* the one to reach for when several services need to share one
index.

## Actions

| Method | Title | Does |
|--------|-------|------|
| `zvec.collection.create` | Create collection | Declare a collection's schema and create it. |
| `zvec.collection.list` | List collections | Name every collection in the data directory. |
| `zvec.collection.info` | Collection info | Document count, index completeness, fields. |
| `zvec.collection.optimize` | Optimize collection | Flush and merge segments; finish the indexes. |
| `zvec.collection.drop` | Drop collection | Delete a collection and its data. |
| `zvec.doc.upsert` | Upsert documents | Write documents, embedding their text. |
| `zvec.doc.search` | Vector search | Nearest neighbours, optionally filtered. |
| `zvec.doc.fts` | Full-text search | Match by word, with boolean expressions. |
| `zvec.doc.hybrid` | Hybrid search | Vector + full text, fused with RRF or weights. |
| `zvec.doc.fetch` | Fetch documents | Read documents by key. |
| `zvec.doc.scan` | Scan documents | Walk a collection a page at a time. |
| `zvec.doc.delete` | Delete documents | Delete by key, or by filter. |

Plus four meta functions behind the form buttons: `zvec.meta.ping` (**Check**
the storage), `zvec.meta.embed` (**Test embedding**), `zvec.meta.collections`
and `zvec.meta.fields` (the ↻ pickers).

## The native library

zvec's engine is C++. The Go binding has two backends, and **this plugin uses
the runtime-FFI one**: it builds with `CGO_ENABLED=0` — no C compiler, no
headers, no pkg-config, on any machine with a Go toolchain — and loads the
engine's shared library at start-up instead.

So the only external dependency is that one library, and `make deps` fetches it:

```bash
make deps     # downloads the engine for this platform into ./lib
make build    # CGO_ENABLED=0 go build
make run      # both, then runs the plugin
```

`scripts/fetch-zvec-lib.sh` picks the right prebuilt release for the host —
Linux (glibc or musl), macOS, Windows, on x86-64 or ARM64 — and unpacks it into
`lib/<goos>_<goarch>/`, which is one of the paths the loader searches. Nothing
needs configuring afterwards.

The plugin looks for the library, in order, in:

1. `ZVEC_LIBRARY_PATH` (a file, or a directory holding it)
2. the working directory, and `./lib/<goos>_<goarch>/` under it
3. the directory holding the binary, and `lib/<goos>_<goarch>/` under that
4. the system library path

So `make dist` lays out a directory that runs anywhere:

```
dist/
  zvec-plugin
  lib/linux_amd64/libzvec_c_api.so
  .env.inflow
```

Cross-fetching for another host works too — `./scripts/fetch-zvec-lib.sh -o
linux -a arm64`, or `-l musl` for Alpine. The `Dockerfile` does the same thing
and produces an image with the engine already beside the binary.

> **Why not a static `.a`?** Upstream publishes shared libraries only
> (`.so`/`.dylib`/`.dll`), and static linking would put cgo and a C++ toolchain
> back on every build host — the opposite of building anywhere. The library is
> also ~16 MB, so it is downloaded rather than committed: `lib/` is gitignored.
> If you need a fully sealed artifact, `make dist` or the Docker image carries
> the library with the binary.

**If you see `zvec/c_api.h: No such file or directory`**, something took the
cgo path — `go run .` and `go build ./...` do, because cgo is on by default.
Use `make run` / `make build`, or set `CGO_ENABLED=0`, or pass `-tags purego`.
The headers that error asks for are only needed by the backend this plugin does
not use.

A plugin whose engine is missing still **starts and serves its forms**, and
fails each action with a message naming the fix — so a node drawer can be opened
and read on a host that is not set up yet.

## Credentials and storage

The plugin holds **no** configuration. A data directory is set per account in
the node's **settings** (alongside the optional embedding provider), the
platform stores it as a named profile, and the runtime folds it into every call
as `body.settings`. One process serves many profiles.

Because the engine takes an **exclusive lock** on each collection directory,
collections are opened once and kept open for the life of the process, and
**one plugin process per data directory** is the rule. A second process writing
to the same directory will fail on the lock.

## Embeddings

zvec stores whatever vectors it is given. Configure a provider — OpenAI, Gemini,
Cohere, or any OpenAI-compatible endpoint (Ollama, LM Studio, vLLM) — and
Upsert, Vector search and Hybrid search accept plain text. **Test embedding**
reports the model's dimension, which is the **Vector size** a collection must be
created with.

## Hybrid search

The reason to pick zvec over a plain vector store. A vector half and a full-text
half each produce a ranking, and the engine fuses them:

- **RRF** merges by rank position, so the halves' scores never have to be
  comparable. The default, and the right starting point.
- **Weighted** scales the raw scores by a weight per half.

One **Query text** drives both halves; either can be overridden. If the
collection has only one half, the action runs as that half's own search instead
of failing.

## Develop

```bash
cp .env.inflow.example .env.inflow    # fill PLUGIN_ID / INFRA_CRED / INFRA_URL
make deps                             # fetch the engine
make run
```

`PLUGIN_ID`, `INFRA_CRED` (base64) and `INFRA_URL` are minted by Infra when the
plugin is defined in a space — see
[getting-started](https://github.com/Inflowenger/getting-started). On startup
the SDK logs each subscribed subject; add the node to a flow to exercise it.

```bash
make test              # everything that needs no engine
make test-integration  # also the tests against the real engine
make check             # fmt, vet, test
```

The integration tests cover the behaviours that are only discoverable by
running the engine — the collection lock, the filter grammar, vector/full-text/
hybrid search, paging and the write modes — and are tagged `integration` so a
machine without the library can still run the rest.

Operator knobs, all optional, all environment variables because they configure
the engine once per process and so cannot be per-profile:

| Variable | Meaning |
|----------|---------|
| `ZVEC_LIBRARY_PATH` | Where the engine's shared library is. |
| `ZVEC_MEMORY_LIMIT_MB` | Engine memory limit. |
| `ZVEC_QUERY_THREADS` | Threads used for queries. |
| `ZVEC_OPTIMIZE_THREADS` | Threads used for optimization. |
| `ZVEC_LOG_LEVEL` | `debug`, `info`, `warn`, `error`, `fatal`. |
| `REQ_TIMEOUT` | NATS request/reply deadline, seconds (default 60). |

## Known limits

- **Group-by search** (one hit per group) exists in the engine but `zvec-go`
  exposes no way to execute the query it builds, so the plugin does not offer
  it.
- **Scan** cannot resume a cursor across flow runs — the engine's iterator is
  live — so paging means skipping what was already seen. Filter on an indexed
  field instead of scanning a large collection.
- A collection's **schema is fixed** at creation: the vector size, metric and
  index cannot be changed afterwards.
- A **filtered delete** does not report how many documents it removed; the
  engine does not say.
