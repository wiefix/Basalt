package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"mime"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/wiefix/basalt/internal/buildinfo"
)

// router registers handlers and remembers each route with who may call
// it, for the API description (/api/v1/openapi.json).
type router struct {
	mux    *http.ServeMux
	routes []route
	// check, when set (tests), wraps each handler to compare its responses
	// with the description.
	check func(method, path string, h http.Handler) http.Handler
}

type route struct {
	method, path string
	access       *access // nil: public
}

func (rt *router) Handle(pattern string, h http.Handler) {
	var a *access
	if g, ok := h.(guarded); ok {
		a = &g.access
	}
	rt.record(pattern, a)
	rt.mux.Handle(pattern, rt.wrap(pattern, h))
}

func (rt *router) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	rt.record(pattern, nil)
	rt.mux.Handle(pattern, rt.wrap(pattern, http.HandlerFunc(h)))
}

func (rt *router) wrap(pattern string, h http.Handler) http.Handler {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok || rt.check == nil || !strings.HasPrefix(path, "/api/") {
		return h
	}
	return rt.check(method, path, h)
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
	// resp is the JSON response: a zero value of its type, an obj(...) for
	// a response built as a map, or noContent (204). Downloads and event
	// streams have none.
	resp    any
	query   []queryParam
	content string // "upload", "download" or "stream" for non-JSON bodies
}

type queryParam struct{ name, help string }

func q(name, help string) queryParam { return queryParam{name, help} }

// noContent is the response of routes that answer 204 without a body.
type noContentResp struct{}

var noContent = noContentResp{}

// object describes a JSON object built as a map: its properties in order.
type object []prop

type prop struct {
	name     string
	optional bool
	value    any
}

// obj describes an object from name, value pairs. A value is a zero value
// of the property's type (a typed nil pointer: it may be null) or another
// obj; a name ending in "?" marks a property that may be absent.
func obj(kv ...any) object {
	o := make(object, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		name := kv[i].(string)
		p := prop{name: strings.TrimSuffix(name, "?"), optional: strings.HasSuffix(name, "?"), value: kv[i+1]}
		o = append(o, p)
	}
	return o
}

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
	gen := &schemaGen{schemas: map[string]any{}, reqNames: map[string]bool{}}
	paths := map[string]map[string]any{}
	// Request bodies first: their components keep their names, a response
	// type of the same name becomes <Name>Out (it lists required fields).
	type described struct {
		rt  route
		doc apiDoc
		op  map[string]any
	}
	var ops []described
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
				"application/json": map[string]any{"schema": gen.value(doc.body)}}}
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
		ops = append(ops, described{rt, doc, op})
	}
	gen.resp = true
	for _, d := range ops {
		var success map[string]any
		code := "2XX"
		switch {
		case d.doc.content == "download":
			success = map[string]any{"description": "The file", "content": map[string]any{
				"application/octet-stream": map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}}}}
		case d.doc.content == "stream":
			success = map[string]any{"description": "Server-sent events (log chunks, then an end event)",
				"content": map[string]any{"text/event-stream": map[string]any{"schema": map[string]any{"type": "string"}}}}
		case d.doc.resp == noContent:
			code, success = "204", map[string]any{"description": "Done (no content)"}
		case d.doc.resp != nil:
			success = map[string]any{"description": "Success", "content": map[string]any{
				"application/json": map[string]any{"schema": gen.value(d.doc.resp)}}}
		default:
			success = map[string]any{"description": "Success"}
		}
		d.op["responses"] = map[string]any{
			code:      success,
			"default": map[string]any{"description": "Error", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}}}},
		}
		if paths[d.rt.path] == nil {
			paths[d.rt.path] = map[string]any{}
		}
		paths[d.rt.path][strings.ToLower(d.rt.method)] = d.op
	}
	gen.schemas["Error"] = map[string]any{"type": "object", "properties": map[string]any{"error": map[string]any{"type": "string"}},
		"required": []string{"error"}, "additionalProperties": false}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "Basalt API",
			"version": buildinfo.Version,
			"description": "The API the panel's web interface uses. Authenticate with an API token " +
				"(Authorization: Bearer bst_…, created under Account) or a session cookie. " +
				"Errors are JSON objects {\"error\": \"…\"}. Unknown fields in request bodies are refused. " +
				"Responses list their required fields; others may be absent.",
		},
		"servers":  []any{map[string]any{"url": "/"}},
		"security": []any{map[string]any{"bearer": []string{}}, map[string]any{"session": []string{}}},
		"paths":    paths,
		"components": map[string]any{
			"schemas": gen.schemas,
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

// schemaGen describes Go types as JSON Schema, named structs as
// components. For responses (resp) it also says which fields are always
// there and which may be null, as encoding/json writes them.
type schemaGen struct {
	schemas  map[string]any
	resp     bool
	reqNames map[string]bool // components made for request bodies
}

// value describes a doc value: a zero value of a type, or an obj(...).
func (g *schemaGen) value(v any) map[string]any {
	if o, ok := v.(object); ok {
		props := map[string]any{}
		required := []string{}
		for _, p := range o {
			s := g.value(p.value)
			if _, isObj := p.value.(object); !isObj && reflect.TypeOf(p.value).Kind() == reflect.Pointer {
				s = nullable(s)
			}
			props[p.name] = s
			if !p.optional {
				required = append(required, p.name)
			}
		}
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	return g.of(reflect.TypeOf(v))
}

// nullable lets s also be null.
func nullable(s map[string]any) map[string]any {
	switch t := s["type"].(type) {
	case string:
		c := map[string]any{}
		for k, v := range s {
			c[k] = v
		}
		c["type"] = []any{t, "null"}
		return c
	case nil:
		if len(s) == 0 {
			return s // anything
		}
	}
	return map[string]any{"oneOf": []any{s, map[string]any{"type": "null"}}}
}

func (g *schemaGen) of(t reflect.Type) map[string]any {
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}
	case t == rawType:
		return map[string]any{} // any JSON
	case t.Kind() == reflect.Pointer:
		return g.of(t.Elem())
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
		return map[string]any{"type": "array", "items": g.of(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": g.of(t.Elem())}
	case reflect.Struct:
		name := schemaName(t)
		if name == "" {
			return g.structSchema(t)
		}
		if g.resp && g.reqNames[name] {
			name += "Out"
		} else if !g.resp {
			g.reqNames[name] = true
		}
		if _, ok := g.schemas[name]; !ok {
			g.schemas[name] = map[string]any{} // placeholder against cycles
			g.schemas[name] = g.structSchema(t)
		}
		return map[string]any{"$ref": "#/components/schemas/" + name}
	}
	return map[string]any{} // interfaces: anything
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

func (g *schemaGen) structSchema(t reflect.Type) map[string]any {
	props := map[string]any{}
	var required []string
	g.addFields(t, props, &required)
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if g.resp {
		slices.Sort(required)
		s["required"] = nonNil(required)
	}
	return s
}

// addFields adds a struct's JSON fields, flattening embedded structs. A
// response field without omitempty is always there; a pointer one may be
// null.
func (g *schemaGen) addFields(t reflect.Type, props map[string]any, required *[]string) {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" || (!f.IsExported() && !f.Anonymous) {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				g.addFields(ft, props, required)
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		omit := strings.Contains(","+opts+",", ",omitempty,") || strings.Contains(","+opts+",", ",omitzero,")
		if hint, ok := schemaHints[f.Tag.Get("openapi")]; ok {
			props[name] = map[string]any{"oneOf": []any{g.of(reflect.TypeOf(hint)), map[string]any{"type": "null"}}}
			continue
		}
		s := g.of(f.Type)
		if g.resp && !omit {
			*required = append(*required, name)
			if f.Type.Kind() == reflect.Pointer || f.Type.Kind() == reflect.Interface {
				s = nullable(s)
			}
		}
		props[name] = s
	}
}

// --- checking responses against the description (tests) ---

// captureWriter keeps a JSON response's body to check it.
type captureWriter struct {
	http.ResponseWriter
	status int
	json   bool
	body   bytes.Buffer
}

const maxChecked = 8 << 20

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
		mt, _, _ := mime.ParseMediaType(c.Header().Get("Content-Type"))
		c.json = mt == "application/json"
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.WriteHeader(http.StatusOK)
	}
	if c.json && c.body.Len() < maxChecked {
		c.body.Write(b)
	}
	return c.ResponseWriter.Write(b)
}

func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// responseChecker returns the router hook that compares every response
// with the description and reports differences to fail.
func responseChecker(rt *router, fail func(string)) func(method, path string, h http.Handler) http.Handler {
	var once sync.Once
	var spec map[string]any
	return func(method, path string, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := &captureWriter{ResponseWriter: w}
			h.ServeHTTP(c, r)
			once.Do(func() {
				b, _ := json.Marshal(openAPI(rt.routes))
				json.Unmarshal(b, &spec)
			})
			if msg := checkResponse(spec, method, path, c); msg != "" {
				fail(fmt.Sprintf("%s %s: response %d does not match the API description: %s", method, path, c.status, msg))
			}
		})
	}
}

func checkResponse(spec map[string]any, method, path string, c *captureWriter) string {
	op, _ := dig(spec, "paths", path, strings.ToLower(method)).(map[string]any)
	if op == nil {
		return "the route is not described"
	}
	responses, _ := op["responses"].(map[string]any)
	switch {
	case c.status == http.StatusNoContent:
		if responses["204"] == nil {
			return "answered 204, described with a body"
		}
		return ""
	case !c.json:
		return "" // downloads, event streams, the UI
	case c.status >= 400:
		return checkSchema(spec, dig(spec, "components", "schemas", "Error").(map[string]any), c.body.Bytes())
	case c.status >= 200 && c.status < 300:
		if responses["204"] != nil {
			return "answered with a body, described as 204"
		}
		schema, _ := dig(responses, "2XX", "content", "application/json", "schema").(map[string]any)
		if schema == nil {
			return "the response is not described"
		}
		return checkSchema(spec, schema, c.body.Bytes())
	}
	return ""
}

func dig(v any, keys ...string) any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func checkSchema(spec, schema map[string]any, body []byte) string {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return "invalid JSON: " + err.Error()
	}
	var errs []string
	validate(spec, schema, v, "$", &errs)
	if len(errs) > 5 {
		errs = append(errs[:5], fmt.Sprintf("and %d more", len(errs)-5))
	}
	return strings.Join(errs, "; ")
}

// validate checks v against the subset of JSON Schema the description uses.
func validate(spec, s map[string]any, v any, at string, errs *[]string) {
	if ref, ok := s["$ref"].(string); ok {
		target, _ := dig(spec, "components", "schemas", strings.TrimPrefix(ref, "#/components/schemas/")).(map[string]any)
		if target == nil {
			*errs = append(*errs, at+": unknown "+ref)
			return
		}
		validate(spec, target, v, at, errs)
		return
	}
	if alts, ok := s["oneOf"].([]any); ok {
		var first []string
		for i, a := range alts {
			var e []string
			validate(spec, a.(map[string]any), v, at, &e)
			if len(e) == 0 {
				return
			}
			if i == 0 {
				first = e
			}
		}
		*errs = append(*errs, first...)
		return
	}
	var types []string
	switch t := s["type"].(type) {
	case string:
		types = []string{t}
	case []any:
		for _, x := range t {
			types = append(types, x.(string))
		}
	default:
		return // anything
	}
	kind := jsonKind(v)
	if !slices.Contains(types, kind) && !(kind == "integer" && slices.Contains(types, "number")) {
		*errs = append(*errs, fmt.Sprintf("%s: %s, want %s", at, kind, strings.Join(types, " or ")))
		return
	}
	switch x := v.(type) {
	case []any:
		if items, ok := s["items"].(map[string]any); ok {
			for i, e := range x {
				validate(spec, items, e, fmt.Sprintf("%s[%d]", at, i), errs)
			}
		}
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		for _, r := range anyStrings(s["required"]) {
			if _, ok := x[r]; !ok {
				*errs = append(*errs, at+"."+r+": missing")
			}
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			if p, ok := props[k].(map[string]any); ok {
				validate(spec, p, x[k], at+"."+k, errs)
				continue
			}
			switch ap := s["additionalProperties"].(type) {
			case bool:
				if !ap {
					*errs = append(*errs, at+"."+k+": not described")
				}
			case map[string]any:
				validate(spec, ap, x[k], at+"."+k, errs)
			}
		}
	}
}

func anyStrings(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, x := range l {
			out = append(out, x.(string))
		}
	}
	return out
}

func jsonKind(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	default:
		return "object"
	}
}
