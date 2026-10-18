package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// tsFile is the TypeScript the web interface checks its API client against.
const tsFile = "../../web/src/api.gen.ts"

// TestTypeScriptTypes keeps web/src/api.gen.ts in line with the API
// description: the schemas and each route's response and body as
// TypeScript types. BASALT_UPDATE=1 rewrites it.
func TestTypeScriptTypes(t *testing.T) {
	e := newEnv(t)
	var spec map[string]any
	raw := e.setupOwner().want(http.StatusOK, "GET", "/api/v1/openapi.json", nil).body
	b, _ := json.Marshal(raw)
	json.Unmarshal(b, &spec)
	got := typeScript(spec)
	if os.Getenv("BASALT_UPDATE") == "1" {
		if err := os.WriteFile(tsFile, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	have, err := os.ReadFile(tsFile)
	if err != nil || string(have) != got {
		abs, _ := filepath.Abs(tsFile)
		t.Fatalf("%s is out of date with the API: run `make api-types` (BASALT_UPDATE=1 go test ./internal/server -run TestTypeScriptTypes)", abs)
	}
}

var tsIdent = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// typeScript writes the description's types as TypeScript.
func typeScript(spec map[string]any) string {
	var b strings.Builder
	b.WriteString("// Generated from the API description (/api/v1/openapi.json) by TestTypeScriptTypes in\n" +
		"// internal/server: run `make api-types` after changing the API. Do not edit.\n" +
		"//\n// <Operation>Response is what a route answers, <Operation>Body what it reads;\n" +
		"// api.check.ts checks the client in api.ts against them.\n")
	schemas := dig(spec, "components", "schemas").(map[string]any)
	for _, name := range slices.Sorted(mapKeys(schemas)) {
		s := schemas[name].(map[string]any)
		if _, isObj := s["properties"]; isObj {
			fmt.Fprintf(&b, "\nexport interface %s %s\n", name, tsType(s, ""))
		} else {
			fmt.Fprintf(&b, "\nexport type %s = %s;\n", name, tsType(s, ""))
		}
	}
	type op struct{ id, resp, body string }
	var ops []op
	for path, methods := range spec["paths"].(map[string]any) {
		for _, o := range methods.(map[string]any) {
			o := o.(map[string]any)
			id := pascal(o["operationId"].(string))
			x := op{id: id}
			if s, ok := dig(o, "responses", "2XX", "content", "application/json", "schema").(map[string]any); ok {
				x.resp = tsType(s, "")
			} else if dig(o, "responses", "204") != nil {
				x.resp = "void"
			}
			if s, ok := dig(o, "requestBody", "content", "application/json", "schema").(map[string]any); ok {
				x.body = tsType(s, "")
			}
			_ = path
			ops = append(ops, x)
		}
	}
	slices.SortFunc(ops, func(a, b op) int { return strings.Compare(a.id, b.id) })
	b.WriteString("\n// Responses and bodies of the routes.\n")
	for _, o := range ops {
		if o.resp != "" {
			fmt.Fprintf(&b, "\nexport type %sResponse = %s;\n", o.id, o.resp)
		}
		if o.body != "" {
			fmt.Fprintf(&b, "\nexport type %sBody = %s;\n", o.id, o.body)
		}
	}
	return b.String()
}

func mapKeys(m map[string]any) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func pascal(s string) string {
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// tsType writes a schema as a TypeScript type; ind is the indentation of
// the line it starts on.
func tsType(s map[string]any, ind string) string {
	if ref, ok := s["$ref"].(string); ok {
		return strings.TrimPrefix(ref, "#/components/schemas/")
	}
	if alts, ok := s["oneOf"].([]any); ok {
		var parts []string
		for _, a := range alts {
			parts = append(parts, tsType(a.(map[string]any), ind))
		}
		return strings.Join(parts, " | ")
	}
	if ts, ok := s["type"].([]any); ok {
		var parts []string
		for _, t := range ts {
			c := map[string]any{}
			for k, v := range s {
				c[k] = v
			}
			c["type"] = t
			parts = append(parts, tsType(c, ind))
		}
		return strings.Join(parts, " | ")
	}
	switch s["type"] {
	case "string":
		return "string"
	case "integer", "number":
		return "number"
	case "boolean":
		return "boolean"
	case "null":
		return "null"
	case "array":
		item := tsType(s["items"].(map[string]any), ind)
		if strings.Contains(item, " | ") {
			item = "(" + item + ")"
		}
		return item + "[]"
	case "object":
		props, _ := s["properties"].(map[string]any)
		if len(props) == 0 {
			switch ap := s["additionalProperties"].(type) {
			case map[string]any:
				return "Record<string, " + tsType(ap, ind) + ">"
			case bool:
				if !ap {
					return "Record<string, never>"
				}
			}
			return "Record<string, unknown>"
		}
		// Without a list of required properties (request bodies) every
		// property is optional.
		req, hasReq := s["required"].([]any)
		required := map[string]bool{}
		for _, r := range req {
			required[r.(string)] = true
		}
		var b strings.Builder
		b.WriteString("{\n")
		for _, k := range slices.Sorted(mapKeys(props)) {
			name := k
			if !tsIdent.MatchString(k) {
				name = fmt.Sprintf("%q", k)
			}
			opt := "?"
			if hasReq && required[k] {
				opt = ""
			}
			fmt.Fprintf(&b, "%s  %s%s: %s;\n", ind, name, opt, tsType(props[k].(map[string]any), ind+"  "))
		}
		b.WriteString(ind + "}")
		return b.String()
	}
	return "unknown"
}
