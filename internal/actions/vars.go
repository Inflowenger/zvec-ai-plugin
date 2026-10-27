package actions

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

// {{ $.a.b }} — capture the JSON path inside the mustaches.
var varRe = regexp.MustCompile(`\{\{\s*(\$[^}]+?)\s*\}\}`)

// varResolver rewrites {{$...}} tokens in an action's inputs against the flow
// scope, so a collection name, a query, a filter value or a key can reference
// what an upstream node produced. It caches per call, so a path referenced by
// several fields is fetched from the runtime once.
type varResolver struct {
	job   *sdkv1.Job
	cache map[string]string
}

func newVarResolver(job *sdkv1.Job) *varResolver {
	return &varResolver{job: job, cache: make(map[string]string)}
}

// resolve substitutes every token in text. A token the scope cannot supply is
// left verbatim, so nothing is silently dropped — a filter that still reads
// {{$.missing}} says plainly what went wrong.
func (vr *varResolver) resolve(text string) string {
	if !strings.Contains(text, "{{") {
		return text
	}
	return varRe.ReplaceAllStringFunc(text, func(token string) string {
		path := strings.TrimSpace(varRe.FindStringSubmatch(token)[1])
		value, ok := vr.cache[path]
		if !ok {
			value = vr.fetch(path)
			vr.cache[path] = value
		}
		return value
	})
}

// fetch reads a JSON path from the flow context. The reply is JSON: a JSON
// string is unwrapped to its value, and anything else is inlined raw so a
// number stays a number and an object can be dropped into a JSON field.
//
// With no flow to ask — which is the case in a unit test, and nowhere else —
// the token is left as it stands, the same answer a path the scope cannot
// supply gets.
func (vr *varResolver) fetch(jsonPath string) string {
	if vr.job == nil {
		return fmt.Sprintf("{{%s}}", jsonPath)
	}
	raw, ok := vr.job.CmdGetScope(jsonPath).([]byte)
	if !ok || len(raw) == 0 {
		return fmt.Sprintf("{{%s}}", jsonPath)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return string(raw)
}

// resolveInputVars rewrites tokens in every string an action's input holds, at
// any depth: the plain fields, the halves of each key/value row, the entries of
// a list, and the text inside a JSON box whether it arrived as text or already
// parsed.
//
// Descending everywhere is what makes the feature uniform — there is no field
// where a token quietly does not work — and it is cheap, because a string with
// no "{{" never reaches the runtime.
func resolveInputVars(job *sdkv1.Job, in any) {
	value := reflect.ValueOf(in)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return
	}
	resolveValue(value.Elem(), newVarResolver(job))
}

// resolveValue rewrites tokens in one value in place. Values reached through an
// interface or a map cannot be written to in place, so those are rebuilt by
// resolveAny and assigned back.
func resolveValue(value reflect.Value, vr *varResolver) {
	switch value.Kind() {
	case reflect.String:
		if value.CanSet() {
			value.SetString(vr.resolve(value.String()))
		}
	case reflect.Struct:
		for i := range value.NumField() {
			resolveValue(value.Field(i), vr)
		}
	case reflect.Slice, reflect.Array:
		for i := range value.Len() {
			resolveValue(value.Index(i), vr)
		}
	case reflect.Pointer:
		if !value.IsNil() {
			resolveValue(value.Elem(), vr)
		}
	case reflect.Interface, reflect.Map:
		if value.CanSet() && !value.IsNil() {
			rebuilt := resolveAny(value.Interface(), vr)
			if rebuilt == nil {
				return
			}
			if converted := reflect.ValueOf(rebuilt); converted.Type().AssignableTo(value.Type()) {
				value.Set(converted)
			}
		}
	}
}

// resolveAny rebuilds a decoded JSON value with its strings resolved. It is
// used for the parts of an input that are map- or interface-shaped — a JSON
// field the platform already decoded, or a key/value row.
func resolveAny(value any, vr *varResolver) any {
	switch typed := value.(type) {
	case string:
		return vr.resolve(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = resolveAny(item, vr)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[vr.resolve(key)] = resolveAny(item, vr)
		}
		return out
	default:
		return value
	}
}
