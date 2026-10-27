package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Inflowenger/go-plugin-sdk/formkit"
	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
	"github.com/Inflowenger/zvec-plugin/internal/embed"
	"github.com/Inflowenger/zvec-plugin/internal/zvecdb"
)

// metaTimeout keeps the meta RPCs snappy: they answer a dialog a user is
// waiting in front of, so a slow embedding provider should fail fast rather
// than hang the drawer.
const metaTimeout = 20 * time.Second

// Metas are the synchronous helpers the forms call while a node is being
// configured — outside the job lifecycle, with no progress and no commit.
func (r *Registry) Metas() []sdkv1.Meta {
	return []sdkv1.Meta{
		{Method: "zvec.meta.ping", RequestHandler: r.metaPing},
		{Method: "zvec.meta.embed", RequestHandler: r.metaEmbed},
		{Method: "zvec.meta.collections", RequestHandler: r.metaCollections},
		{Method: "zvec.meta.fields", RequestHandler: r.metaFields},
	}
}

// Settings declares what a zvec profile must contain. The platform renders this
// form when the plugin is set up, stores the answers as a named profile, and
// ships the values back with every call as body.settings.
//
// The submit handler is a validator, not a store: it checks that the directory
// is usable so a wrong path is reported in the dialog rather than by every node
// that later fails.
func (r *Registry) Settings() *sdkv1.Settings {
	return &sdkv1.Settings{
		FormBuilder: settingsForm,
		SubmitHandler: func(req sdkv1.Request) sdkv1.Response {
			submitted := decodeMeta[map[string]any](req.Data)
			if nested, ok := submitted["settings"].(map[string]any); ok {
				submitted = nested
			}
			cfg, err := zvecdb.ParseConfig(submitted)
			if err != nil {
				return sdkv1.Response{Error: err.Error()}
			}
			if err := cfg.CheckDir(); err != nil {
				return sdkv1.Response{Error: err.Error()}
			}
			names, err := cfg.ListCollections()
			if err != nil {
				return sdkv1.Response{Error: err.Error()}
			}
			return sdkv1.Response{Data: map[string]any{
				"ok":          true,
				"dataDir":     cfg.DataDir,
				"collections": names,
				"engine":      zvecdb.Version(),
			}}
		},
	}
}

// SettingsForm exposes the same form for PluginIntro.Settings — the document
// the platform's plugin set-up dialog reads to build the profile editor.
func (r *Registry) SettingsForm() *sdkv1.FormBuilder {
	form := settingsForm
	return &form
}

// metaPing backs the settings dialog's "Check" button: make sure the data
// directory is usable and say what is already in it.
//
// It also reports whether the engine loaded at all, which is the one failure
// that no amount of correct configuration fixes — and the first thing to look
// at when every action on a fresh install fails the same way.
func (r *Registry) metaPing(req sdkv1.Request) any {
	settings := metaSettings(req)

	if err := zvecdb.InitLibrary(); err != nil {
		return formkit.Failure("%s", err).About("dataDir").Patch(nil)
	}
	cfg, err := zvecdb.ParseConfig(settings)
	if err != nil {
		return formkit.Failure("%s", err).About("dataDir").Patch(nil)
	}
	if err := cfg.CheckDir(); err != nil {
		return formkit.Failure("%s", err).About("dataDir").Patch(nil)
	}
	names, err := cfg.ListCollections()
	if err != nil {
		return formkit.Failure("%s", err).About("dataDir").Patch(nil)
	}

	engine := zvecdb.Version()
	if len(names) == 0 {
		return formkit.Success("Ready — %s is usable and holds no collections yet (zvec %s).", cfg.DataDir, engine).
			About("dataDir").Patch(nil)
	}
	return formkit.Success("Ready — %d collection(s) in %s: %s (zvec %s).", len(names), cfg.DataDir, strings.Join(names, ", "), engine).
		About("dataDir").Patch(nil)
}

// metaEmbed backs the settings dialog's "Test embedding" button: embed a probe
// with the provider in the open form and report the dimension — the number a
// new collection's Vector size has to be.
func (r *Registry) metaEmbed(req sdkv1.Request) any {
	settings := metaSettings(req)
	raw := zvecdb.ParseEmbedding(settings)
	provider := embed.Settings{Provider: raw.Provider, Model: raw.Model, APIKey: raw.APIKey, BaseURL: raw.BaseURL}

	if !provider.Enabled() {
		return formkit.Warning("Pick an embedding provider first, then press ↻.").About("embeddingModel").Patch(nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), metaTimeout)
	defer cancel()

	vector, err := embed.One(ctx, provider, "ping", embed.Query)
	if err != nil {
		return formkit.Failure("Embedding failed: %s", err).About("embeddingModel").Patch(nil)
	}
	return formkit.Success("Embedded a probe — %d dimensions. Use %d as the Vector size when creating a collection.", len(vector), len(vector)).
		About("embeddingModel").Patch(nil)
}

// metaCollections backs the ↻ button on every Collection box: list what is in
// the data directory and rebuild the calling form with that box turned into a
// drop-down. `form` — set by the field's Picks() — names which form to rebuild;
// without a match the names are reported as text instead, which is a worse
// answer than a drop-down but still an answer.
func (r *Registry) metaCollections(req sdkv1.Request) any {
	call := decodeMeta[map[string]any](req.Data)
	settings, _ := call["settings"].(map[string]any)

	if err := zvecdb.InitLibrary(); err != nil {
		return formkit.Failure("%s", err).About("collection").Patch(nil)
	}
	cfg, err := zvecdb.ParseConfig(settings)
	if err != nil {
		return formkit.Failure("%s", err).About("collection").Patch(nil)
	}
	names, err := cfg.ListCollections()
	if err != nil {
		return formkit.Failure("%s", err).About("collection").Patch(nil)
	}
	if len(names) == 0 {
		return formkit.Warning("No collections in %s yet — create one with the Create collection action.", cfg.DataDir).
			About("collection").Patch(nil)
	}

	options := make([]formkit.Option, 0, len(names))
	for _, name := range names {
		options = append(options, formkit.Option{Value: name, Label: name})
	}
	heading := formkit.Success("%d collection(s) in %s — pick one.", len(names), cfg.DataDir)
	target := metaString(call, "targetField")
	if target == "" {
		target = "collection"
	}

	owner, ok := pickableForms[metaString(call, "form")]
	if !ok {
		return formkit.Info("Collections in %s:\n%s", cfg.DataDir, formkit.Lines(options)).About(target).Patch(nil)
	}
	return formkit.Choose(owner, target, options, formkit.FormData(call), heading)
}

// metaFields backs the ↻ on the Vector field and Text field boxes: open the
// collection named in the open form and offer the fields that can go there —
// the vector fields for a vector box, the full-text ones for a text box.
//
// Which box is being filled is read from targetField, so one meta serves both.
func (r *Registry) metaFields(req sdkv1.Request) any {
	call := decodeMeta[map[string]any](req.Data)
	settings, _ := call["settings"].(map[string]any)
	target := metaString(call, "targetField")
	if target == "" {
		target = "vectorField"
	}

	cfg, err := zvecdb.ParseConfig(settings)
	if err != nil {
		return formkit.Failure("%s", err).About(target).Patch(nil)
	}
	name := strings.TrimSpace(metaString(call, "collection"))
	if name == "" {
		return formkit.Info("Fill in the collection first, then press ↻ to list its fields.").About(target).Patch(nil)
	}
	handle, err := r.store.Open(cfg, name)
	if err != nil {
		return formkit.Failure("%s", err).About(target).Patch(nil)
	}
	stats, err := handle.Stats()
	if err != nil {
		return formkit.Failure("%s", err).About(target).Patch(nil)
	}

	wantVector := target == "vectorField"
	options := make([]formkit.Option, 0, len(stats.Fields))
	for _, field := range stats.Fields {
		if wantVector && field.Vector {
			options = append(options, formkit.Option{Value: field.Name, Label: fmt.Sprintf("%s — %s, %d dimensions, %s", field.Name, field.Type, field.Dim, orDefault(field.Index, "no index"))})
		}
		if !wantVector && !field.Vector && field.Index == "FTS" {
			options = append(options, formkit.Option{Value: field.Name, Label: field.Name + " — full-text"})
		}
	}
	if len(options) == 0 {
		kind := "full-text"
		if wantVector {
			kind = "vector"
		}
		return formkit.Warning("%q has no %s field this search can use.", handle.Name, kind).About(target).Patch(nil)
	}

	heading := formkit.Success("%d field(s) on %s — pick one.", len(options), handle.Name)
	owner, ok := pickableForms[metaString(call, "form")]
	if !ok {
		return formkit.Info("Fields on %s:\n%s", handle.Name, formkit.Lines(options)).About(target).Patch(nil)
	}
	return formkit.Choose(owner, target, options, formkit.FormData(call), heading)
}

// metaSettings reads the values a settings-dialog button posts. They arrive one
// of two ways: on an action's drawer the bound profile is under "settings"; the
// plugin set-up dialog has no bound profile and posts the values being edited
// at the top level, beside the keys the host adds.
func metaSettings(req sdkv1.Request) map[string]any {
	body := decodeMeta[map[string]any](req.Data)
	if settings, ok := body["settings"].(map[string]any); ok && len(settings) > 0 {
		return settings
	}
	for _, hostKey := range []string{"settings", "value", "targetField", "form"} {
		delete(body, hostKey)
	}
	return body
}

func metaString(call map[string]any, key string) string {
	if value, ok := call[key].(string); ok {
		return value
	}
	return ""
}

// decodeMeta reads a meta RPC's arguments. A meta call comes from the form
// renderer rather than the job pipeline, so the payload may or may not be
// wrapped in the {_registry, body} envelope — try both, and treat anything
// unreadable as "no arguments" rather than as an error, since there is no error
// channel back to a button.
func decodeMeta[T any](data []byte) T {
	var out T
	if len(bytes.TrimSpace(data)) == 0 {
		return out
	}

	var envelope struct {
		Body json.RawMessage `json:"body"`
	}
	if json.Unmarshal(data, &envelope) == nil && len(envelope.Body) > 0 {
		if json.Unmarshal(envelope.Body, &out) == nil {
			return out
		}
	}
	_ = json.Unmarshal(data, &out)
	return out
}
