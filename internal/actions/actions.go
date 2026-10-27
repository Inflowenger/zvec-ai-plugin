// Package actions wires the embedded zvec engine onto the plugin's node
// actions: five that manage collections and seven that read and write
// documents, plus the meta RPCs the node's forms call while a user is
// configuring them.
package actions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/Inflowenger/zvec-ai-plugin/internal/embed"
	"github.com/Inflowenger/zvec-ai-plugin/internal/zvecdb"
)

// jobTimeout bounds the parts of an action that can hang: embedding a batch of
// text over the network. The engine's own calls are local and not cancellable
// mid-flight, so this is a budget for the provider rather than a kill switch
// for the database.
const jobTimeout = 2 * time.Minute

// Registry owns what the actions share: the pool of open collections. The
// plugin holds no storage configuration of its own — each call brings its own
// data directory in body.settings.
type Registry struct {
	store *zvecdb.Store
}

func New() *Registry { return &Registry{store: zvecdb.NewStore()} }

// Close releases every collection the plugin holds open. Call it on the way
// out, before the engine is shut down.
func (r *Registry) Close() { r.store.Close() }

// All returns every action this plugin exposes, in the order the canvas lists
// them: the collection lifecycle first, then the documents.
func (r *Registry) All() []sdkv1.Action {
	return []sdkv1.Action{
		r.createCollection(),
		r.listCollections(),
		r.collectionInfo(),
		r.optimizeCollection(),
		r.dropCollection(),
		r.upsert(),
		r.search(),
		r.fullText(),
		r.hybrid(),
		r.fetch(),
		r.scan(),
		r.deleteDocs(),
	}
}

// connection is the platform-managed half of every request body. The runtime
// resolves the settings profile bound to the node and folds it in as
// body.settings, so an action's own input struct declares only its own fields.
type connection struct {
	Settings map[string]any `json:"settings"`
}

// env is the resolved connection one action runs against.
type env struct {
	cfg zvecdb.Config
	emb embed.Settings
}

// envFor reads the settings profile out of the same bytes the action's input
// came from, which keeps the settings envelope out of every input struct.
func envFor(data []byte) (env, error) {
	conn, err := sdkv1.CastRequestTo[connection](data)
	if err != nil {
		return env{}, fmt.Errorf("invalid request body: %w", err)
	}
	cfg, err := zvecdb.ParseConfig(conn.Body.Settings)
	if err != nil {
		return env{}, err
	}
	raw := zvecdb.ParseEmbedding(conn.Body.Settings)
	return env{
		cfg: cfg,
		emb: embed.Settings{
			Provider: raw.Provider,
			Model:    raw.Model,
			APIKey:   raw.APIKey,
			BaseURL:  raw.BaseURL,
		},
	}, nil
}

// target is the collection an action acts on, embedded by every input that
// names one.
type target struct {
	Collection string `json:"collection"`
}

func (t target) collection() string { return t.Collection }

// collectionInput is an action input that names a collection.
type collectionInput interface {
	collection() string
}

// handler is what an action implements: work over a resolved connection,
// returning what to commit. Finishing the job is run's business, so a handler
// has exactly one way to fail — return an error — and cannot forget to
// terminate.
type handler[T any] func(ctx context.Context, job *sdkv1.Job, e env, in T) (map[string]any, error)

// run adapts a handler into an SDK job handler: decode the typed body and the
// connection that came with it, resolve {{$...}} tokens against the flow,
// report progress, and terminate the job exactly once on every path.
func run[T any](r *Registry, title string, fn handler[T]) sdkv1.JobHandler {
	return func(job sdkv1.Job) {
		req, err := sdkv1.CastRequestTo[T](job.Req.Data)
		if err != nil {
			job.DoneWithError("invalid request body: " + err.Error())
			return
		}
		e, err := envFor(job.Req.Data)
		if err != nil {
			job.DoneWithError(err.Error())
			return
		}

		resolveInputVars(&job, &req.Body)
		job.Progress(20, sdkv1.Frame{Title: title, Content: e.cfg.Describe()})

		ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
		defer cancel()

		out, err := fn(ctx, &job, e, req.Body)
		if err != nil {
			job.DoneWithError(err.Error())
			return
		}
		if out == nil {
			out = map[string]any{}
		}
		job.Done(out)
	}
}

// onCollection is run for the actions that act on one named collection: it
// opens the collection — and therefore reports a missing one, or a locked data
// directory, before the action's own logic has to care.
func onCollection[T collectionInput](r *Registry, title string, fn func(ctx context.Context, job *sdkv1.Job, e env, h *zvecdb.Handle, in T) (map[string]any, error)) sdkv1.JobHandler {
	return run(r, title, func(ctx context.Context, job *sdkv1.Job, e env, in T) (map[string]any, error) {
		handle, err := r.store.Open(e.cfg, in.collection())
		if err != nil {
			return nil, err
		}
		job.Progress(40, sdkv1.Frame{Title: title, Content: handle.Name})
		return fn(ctx, job, e, handle, in)
	})
}

// ------------------------------------------------------------- embedding --

// embedQuery turns a query into a vector: an explicit vector wins, text is
// embedded, and neither is an error only where the caller allows it.
//
// A query and a stored document are embedded with different instructions by
// the providers that distinguish them, so this is the one place a query may be
// embedded from and the Purpose is not a parameter the callers can get wrong.
func (r *Registry) embedQuery(ctx context.Context, e env, text string, rawVector any) ([]float32, error) {
	if rawVector != nil {
		vector, err := zvecdb.Vector(rawVector, "vector")
		if err == nil {
			if strings.TrimSpace(text) != "" {
				return nil, errors.New("give either a query text or a query vector, not both: the text would be embedded into a second, different vector")
			}
			return vector, nil
		}
		// An empty JSON box is not a vector and not a mistake; anything else is.
		if !isEmptyValue(rawVector) {
			return nil, err
		}
	}
	if text = strings.TrimSpace(text); text == "" {
		return nil, nil
	}
	if !e.emb.Enabled() {
		return nil, fmt.Errorf("%w (the query text would have to be embedded to be searched)", embed.ErrNoProvider)
	}
	vector, err := embed.One(ctx, e.emb, text, embed.Query)
	if err != nil {
		return nil, fmt.Errorf("cannot embed the query: %w", err)
	}
	return vector, nil
}

// isEmptyValue reports whether a JSON-ish form field was left blank, which an
// optional box is allowed to be.
func isEmptyValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(typed) == ""
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}
