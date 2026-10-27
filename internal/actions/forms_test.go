package actions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Inflowenger/go-plugin-sdk/sdkv1"
)

// registry is a plugin with no engine behind it, which is all these tests need:
// a form is declared at start-up and never touches storage.
func registry(t *testing.T) *Registry {
	t.Helper()
	r := New()
	t.Cleanup(r.Close)
	return r
}

func TestEveryActionIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, action := range registry(t).All() {
		t.Run(action.Method, func(t *testing.T) {
			if action.Method == "" || action.Title == "" || action.Description == "" {
				t.Fatalf("action is missing a method, title or description: %+v", action)
			}
			if seen[action.Method] {
				t.Fatalf("two actions share the method %q", action.Method)
			}
			seen[action.Method] = true
			if action.RequestHandler == nil {
				t.Fatal("action has no request handler, so the node would hang")
			}
			if !strings.HasPrefix(action.Method, "zvec.") {
				t.Fatalf("method %q is outside the plugin's namespace", action.Method)
			}
			parseSchema(t, action.Form.Jsonschema)
			parseUI(t, action.Form.Jsonui)
		})
	}
}

// A connection is the platform's half of the body: it is folded in as
// body.settings from the bound profile, so an action that also asked for it on
// its own form would be asking the user to retype a credential per node.
func TestNoActionFormAsksForSettings(t *testing.T) {
	for _, action := range registry(t).All() {
		schema := parseSchema(t, action.Form.Jsonschema)
		properties, _ := schema["properties"].(map[string]any)
		for _, forbidden := range []string{"settings", "dataDir", "embeddingApiKey"} {
			if _, found := properties[forbidden]; found {
				t.Errorf("%s declares %q on its own form; it belongs to the settings profile", action.Method, forbidden)
			}
		}
	}
}

// Every button on a form calls a meta method by name. A name with no handler
// behind it fails silently in the drawer — the button simply does nothing — so
// the wiring is checked here instead.
func TestEveryLookupNamesARegisteredMeta(t *testing.T) {
	r := registry(t)
	registered := map[string]bool{}
	for _, meta := range r.Metas() {
		registered[meta.Method] = true
	}

	forms := map[string]sdkv1.FormBuilder{"<settings>": settingsForm}
	for _, action := range r.All() {
		forms[action.Method] = action.Form
	}
	for owner, form := range forms {
		for _, call := range lookups(t, form.Jsonui) {
			if !registered[call.fn] {
				t.Errorf("%s has a button calling %q, which is not a registered meta method", owner, call.fn)
			}
			if call.name != "pluginFn" {
				t.Errorf("%s has a button whose action name is %q; the host only knows pluginFn", owner, call.name)
			}
			if call.form != "" {
				if _, ok := pickableForms[call.form]; !ok {
					t.Errorf("%s has a button that picks into %q, which is not in pickableForms, so an ambiguous lookup cannot rebuild it", owner, call.form)
				}
			}
		}
	}
}

// The settings form's SubmitTo is served by the settings submit handler, not by
// AddMeta. Naming a registered meta there would subscribe two handlers to one
// subject and let the race decide which answered.
func TestSettingsSubmitSubjectIsItsOwn(t *testing.T) {
	r := registry(t)
	if settingsForm.SubmitTo == "" {
		t.Fatal("the settings form names no submit subject")
	}
	for _, meta := range r.Metas() {
		if meta.Method == settingsForm.SubmitTo {
			t.Fatalf("the settings submit subject %q is also a registered meta method: both would subscribe to it", meta.Method)
		}
	}
	if r.Settings().SubmitHandler == nil {
		t.Fatal("the settings profile has no submit handler, so the set-up dialog cannot validate")
	}
}

// Every control in a UI schema must point at a property the data schema
// declares; one that does not simply stops rendering.
func TestEveryControlPointsAtAProperty(t *testing.T) {
	r := registry(t)
	forms := map[string]sdkv1.FormBuilder{"<settings>": settingsForm}
	for _, action := range r.All() {
		forms[action.Method] = action.Form
	}

	for owner, form := range forms {
		schema := parseSchema(t, form.Jsonschema)
		properties, _ := schema["properties"].(map[string]any)
		for _, scope := range scopes(t, form.Jsonui) {
			name := strings.TrimPrefix(scope, "#/properties/")
			if _, ok := properties[name]; !ok {
				t.Errorf("%s has a control for %q, which the schema does not declare", owner, name)
			}
		}
		// And the other way: a property with no control is a field a user
		// cannot fill in.
		rendered := map[string]bool{}
		for _, scope := range scopes(t, form.Jsonui) {
			rendered[strings.TrimPrefix(scope, "#/properties/")] = true
		}
		for name := range properties {
			if !rendered[name] {
				t.Errorf("%s declares the property %q but renders no control for it", owner, name)
			}
		}
	}
}

func TestSettingsFormAsksForTheDataDirectory(t *testing.T) {
	schema := parseSchema(t, settingsForm.Jsonschema)
	properties, _ := schema["properties"].(map[string]any)
	if _, ok := properties["dataDir"]; !ok {
		t.Fatal("the settings form does not ask for a data directory")
	}
	required, _ := schema["required"].([]any)
	var found bool
	for _, name := range required {
		if name == "dataDir" {
			found = true
		}
	}
	if !found {
		t.Error("dataDir is not required, so a profile could be saved with nowhere to put the data")
	}
}

// ------------------------------------------------------------- helpers --

func parseSchema(t *testing.T, text string) map[string]any {
	t.Helper()
	if strings.TrimSpace(text) == "" {
		t.Fatal("form has an empty JSON Schema")
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(text), &schema); err != nil {
		t.Fatalf("JSON Schema does not parse: %v", err)
	}
	if schema["type"] != "object" {
		t.Fatalf("JSON Schema is not an object schema: %v", schema["type"])
	}
	return schema
}

func parseUI(t *testing.T, text string) map[string]any {
	t.Helper()
	if strings.TrimSpace(text) == "" {
		t.Fatal("form has an empty UI schema")
	}
	var ui map[string]any
	if err := json.Unmarshal([]byte(text), &ui); err != nil {
		t.Fatalf("UI schema does not parse: %v", err)
	}
	return ui
}

type lookupCall struct{ name, fn, form string }

// lookups walks a UI schema for the buttons an x-inflow-ui extension hangs off
// its controls.
func lookups(t *testing.T, text string) []lookupCall {
	t.Helper()
	var found []lookupCall
	walkUI(parseUI(t, text), func(element map[string]any) {
		ui, ok := element["x-inflow-ui"].(map[string]any)
		if !ok {
			return
		}
		action, _ := ui["action"].(map[string]any)
		if action == nil {
			return
		}
		call := lookupCall{}
		call.name, _ = action["name"].(string)
		call.fn, _ = action["fn"].(string)
		if body, ok := action["body"].(map[string]any); ok {
			call.form, _ = body["form"].(string)
		}
		found = append(found, call)
	})
	return found
}

func scopes(t *testing.T, text string) []string {
	t.Helper()
	var found []string
	walkUI(parseUI(t, text), func(element map[string]any) {
		if element["type"] != "Control" {
			return
		}
		if scope, ok := element["scope"].(string); ok {
			found = append(found, scope)
		}
	})
	return found
}

// walkUI visits every element of a UI schema, descending into layouts.
func walkUI(element map[string]any, visit func(map[string]any)) {
	visit(element)
	children, _ := element["elements"].([]any)
	for _, child := range children {
		if nested, ok := child.(map[string]any); ok {
			walkUI(nested, visit)
		}
	}
}
