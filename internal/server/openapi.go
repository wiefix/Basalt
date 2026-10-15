package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/wiefix/basalt/internal/buildinfo"
)

// router registers handlers and remembers each route with who may call
// it, for the API description (/api/v1/openapi.json).
type router struct {
	mux    *http.ServeMux
	routes []route
}

type route struct {
	method, path string
	access       *access // nil: public
}

func (rt *router) Handle(pattern string, h http.Handler) {
	rt.mux.Handle(pattern, h)
	var a *access
	if g, ok := h.(guarded); ok {
		a = &g.access
	}
	rt.record(pattern, a)
}

func (rt *router) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	rt.mux.HandleFunc(pattern, h)
	rt.record(pattern, nil)
}

func (rt *router) record(pattern string, a *access) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		return // catch-alls
	}
	rt.routes = append(rt.routes, route{method: method, path: path, access: a})
}

// apiDoc describes a route; apiDocs (openapi_docs.go) holds one per route.
type apiDoc struct {
	summary string
	body    any // the JSON request body (a zero value of its type); nil: none
	query   []queryParam
	content string // "upload", "download" or "stream" for non-JSON bodies
}

type queryParam struct{ name, help string }

func q(name, help string) queryParam { return queryParam{name, help} }

// backupTargets are the prefixes the backup routes are registered under;
// their docs use "{target}".
var backupTargets = []string{"/api/v1/sites/{id}", "/api/v1/apps/{id}"}

func docFor(method, path string) (apiDoc, bool) {
	if d, ok := apiDocs[method+" "+path]; ok {
		return d, true
	}
	for _, t := range backupTargets {
		if rest, ok := strings.CutPrefix(path, t); ok {
			d, ok := apiDocs[method+" {target}"+rest]
			return d, ok
		}
	}
	return apiDoc{}, false
}

var pathParam = regexp.MustCompile(`\{(\w+)\}`)

// openAPI builds the OpenAPI 3.1 description of routes.
func openAPI(routes []route) map[string]any {
	schemas := map[string]any{}
	paths := map[string]map[string]any{}
	for _, rt := range routes {
		if !strings.HasPrefix(rt.path, "/api/") {
			continue
		}
		doc, _ := docFor(rt.method, rt.path)
		op := map[string]any{
			"summary":     doc.summary,
			"operationId": operationID(rt.method, rt.path),
			"tags":        []string{apiTag(rt.path)},
		}
		var params []any
		for _, m := range pathParam.FindAllStringSubmatch(rt.path, -1) {
			params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
		}
		for _, p := range doc.query {
			params = append(params, map[string]any{"name": p.name, "in": "query", "description": p.help, "schema": map[string]any{"type": "string"}})
		}
		if params != nil {
			op["parameters"] = params
		}
		switch {
		case doc.content == "upload":
			op["requestBody"] = map[string]any{"required": true, "content": map[string]any{
				"application/octet-stream": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}}}}
		case doc.body != nil:
			op["requestBody"] = map[string]any{"required": true, "content": map[string]any{
				"application/json": map[string]any{"schema": schemaOf(reflect.TypeOf(doc.body), schemas)}}}
		}
		success := map[string]any{"description": "Success", "content": map[string]any{
			"application/json": map[string]any{"schema": map[string]any{"type": "object"}}}}
		switch doc.content {
		case "download":
			success["content"] = map[string]any{"application/octet-stream": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}}}
		case "stream":
			success["description"] = "Server-sent events (log chunks, then an end event)"
			success["content"] = map[string]any{"text/event-stream": map[string]any{"schema": map[string]any{"type": "string"}}}
		}
		op["responses"] = map[string]any{
			"2XX":     success,
			"default": map[string]any{"description": "Error", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}}}},
		}
		var notes []string
		switch a := rt.access; {
		case a == nil:
			op["security"] = []any{}
			notes = append(notes, "No authentication.")
		case a.mfaPending:
			op["security"] = []any{map[string]any{"session": []string{}}}
			notes = append(notes, "A session that passed the password and waits for the second factor.")
		default:
			if a.role != "" {
				op["x-basalt-role"] = a.role
				notes = append(notes, "Role: "+string(a.role)+" or higher.")
			}
			if a.sessionOnly {
				op["security"] = []any{map[string]any{"session": []string{}}}
				notes = append(notes, "Signed-in session only, not an API token.")
			}
		}
		op["description"] = strings.Join(notes, " ")
		if paths[rt.path] == nil {
			paths[rt.path] = map[string]any{}
		}
		paths[rt.path][strings.ToLower(rt.method)] = op
	}
	schemas["Error"] = map[string]any{"type": "object", "properties": map[string]any{"error": map[string]any{"type": "string"}}}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "Basalt API",
			"version": buildinfo.Version,
			"description": "The API the panel's web interface uses. Authenticate with an API token " +
				"(Authorization: Bearer bst_…, created under Account) or a session cookie. " +
				"Errors are JSON objects {\"error\": \"…\"}. Unknown fields in request bodies are refused.",
		},
		"servers":  []any{map[string]any{"url": "/"}},
		"security": []any{map[string]any{"bearer": []string{}}, map[string]any{"session": []string{}}},
		"paths":    paths,
		"components": map[string]any{
			"schemas": schemas,
			"securitySchemes": map[string]any{
				"bearer":  map[string]any{"type": "http", "scheme": "bearer", "description": "API token"},
				"session": map[string]any{"type": "apiKey", "in": "cookie", "name": "__Host-basalt_session"},
			},
		},
	}
}

func (s *Server) serveOpenAPI(w http.ResponseWriter, routes []route) {
	writeJSON(w, http.StatusOK, openAPI(routes))
}

// apiTag groups a route by what it is about.
func apiTag(path string) string {
	rest := strings.TrimPrefix(path, "/api/v1/")
	for _, kw := range []string{"backups", "backup-policy", "snapshots", "restore"} {
		if strings.Contains(rest, "/"+kw) {
			return "backups"
		}
	}
	for _, kw := range []string{"files", "sftp-users", "databases", "deployments", "logs", "replicas", "template"} {
		if strings.Contains(rest, "/"+kw) && !strings.HasPrefix(rest, kw) {
			return kw
		}
	}
	first, _, _ := strings.Cut(rest, "/")
	return first
}

// operationID names a route: GET /api/v1/apps/{id}/logs → getAppsIdLogs.
func operationID(method, path string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for _, part := range strings.FieldsFunc(strings.TrimPrefix(path, "/api/v1/"), func(r rune) bool {
		return r == '/' || r == '-' || r == '{' || r == '}'
	}) {
		r := []rune(part)
		r[0] = unicode.ToUpper(r[0])
		b.WriteString(string(r))
	}
	return b.String()
}

var (
	timeType = reflect.TypeOf(time.Time{})
	rawType  = reflect.TypeOf(json.RawMessage{})
)

// schemaHints are the types fields tagged openapi:"<name>" hold when their
// Go type (json.RawMessage, decoded later) does not say.
var schemaHints = map[string]any{
	"mailRelayRequest": mailRelayRequest{},
}

// schemaOf describes a Go type as JSON Schema; named structs go to schemas.
func schemaOf(t reflect.Type, schemas map[string]any) map[string]any {
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t == rawType:
		return map[string]any{} // any JSON
	case t.Kind() == reflect.Pointer:
		return schemaOf(t.Elem(), schemas)
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string", "contentEncoding": "base64"}
		}
		return map[string]any{"type": "array", "items": schemaOf(t.Elem(), schemas)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaOf(t.Elem(), schemas)}
	case reflect.Struct:
		name := schemaName(t)
		if name == "" {
			return structSchema(t, schemas)
		}
		if _, ok := schemas[name]; !ok {
			schemas[name] = map[string]any{} // placeholder against cycles
			schemas[name] = structSchema(t, schemas)
		}
		return map[string]any{"$ref": "#/components/schemas/" + name}
	}
	return map[string]any{}
}

func schemaName(t reflect.Type) string {
	if t.Name() == "" {
		return ""
	}
	r := []rune(t.Name())
	r[0] = unicode.ToUpper(r[0])
	name := string(r)
	if pkg := t.PkgPath(); !strings.HasSuffix(pkg, "/server") {
		p := []rune(pkg[strings.LastIndex(pkg, "/")+1:])
		p[0] = unicode.ToUpper(p[0])
		name = string(p) + name
	}
	return name
}

func structSchema(t reflect.Type, schemas map[string]any) map[string]any {
	props := map[string]any{}
	addFields(t, schemas, props)
	return map[string]any{"type": "object", "properties": props, "additionalProperties": false}
}

// addFields adds a struct's JSON fields, flattening embedded structs.
func addFields(t reflect.Type, schemas map[string]any, props map[string]any) {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" || (!f.IsExported() && !f.Anonymous) {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				addFields(ft, schemas, props)
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		if hint, ok := schemaHints[f.Tag.Get("openapi")]; ok {
			props[name] = map[string]any{"oneOf": []any{schemaOf(reflect.TypeOf(hint), schemas), map[string]any{"type": "null"}}}
			continue
		}
		props[name] = schemaOf(f.Type, schemas)
	}
}
