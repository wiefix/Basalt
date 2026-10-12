// Package catalog holds the app templates installed in one click: a
// docker-compose file plus metadata (template.yaml) saying what to ask the
// user, which secrets to generate and which service to publish.
//
// An installed template is an ordinary compose app: its inputs and
// secrets become the app's (encrypted) environment, and the compose file
// refers to them as ${KEY}. Templates are checked strictly: every image is
// pinned by digest, every variable is declared, and the compose file must
// pass the same allowlist as any other.
package catalog

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/mail"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"

	"github.com/wiefix/basalt/internal/compose"
	"github.com/wiefix/basalt/internal/proxy"
)

// Input types.
const (
	InputDomain = "domain" // the app's domain; exactly one per template
	InputText   = "text"
	InputEmail  = "email"
)

// Template is an app in the catalog.
type Template struct {
	ID          string `yaml:"id" json:"id"`
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	Category    string `yaml:"category" json:"category"`
	Website     string `yaml:"website" json:"website"`
	// Version is the application's; Revision counts changes to the
	// template (a higher revision is an update).
	Version  string `yaml:"version" json:"version"`
	Revision int    `yaml:"revision" json:"revision"`
	// MemoryMB is the memory the app needs to run comfortably.
	MemoryMB int      `yaml:"memory_mb" json:"memory_mb"`
	Inputs   []Input  `yaml:"inputs" json:"inputs"`
	Secrets  []Secret `yaml:"secrets" json:"secrets"`
	Expose   Expose   `yaml:"expose" json:"expose"`
	// Notes are shown after installation (first login and the like).
	Notes string `yaml:"notes" json:"notes"`

	Compose string `yaml:"-" json:"-"`
	// What the compose file runs, for display.
	Services []Service `yaml:"-" json:"services"`
	Volumes  []string  `yaml:"-" json:"volumes"`
}

// Input is a value asked from the user when installing.
type Input struct {
	Key      string `yaml:"key" json:"key"`
	Label    string `yaml:"label" json:"label"`
	Type     string `yaml:"type" json:"type"`
	Required bool   `yaml:"required" json:"required"`
	Default  string `yaml:"default" json:"default,omitempty"`
	Help     string `yaml:"help" json:"help,omitempty"`
}

// Secret is a value generated when installing (passwords, keys).
type Secret struct {
	Key    string `yaml:"key" json:"key"`
	Length int    `yaml:"length" json:"length"`
	// Charset is "alnum" (default) or "hex".
	Charset string `yaml:"charset" json:"charset,omitempty"`
}

// Expose is the service published on the app's domain.
type Expose struct {
	Service string `yaml:"service" json:"service"`
	Port    int    `yaml:"port" json:"port"`
}

// Service is a service of the compose file and its image.
type Service struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

var (
	idRE   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,40}$`)
	keyRE  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	digest = regexp.MustCompile(`^[^@]+:[^@:/]+@sha256:[0-9a-f]{64}$`)
)

func printable(s string, max int, multiline bool) bool {
	if len(s) > max {
		return false
	}
	for _, r := range s {
		if r < 0x20 && !(multiline && r == '\n') || r == 0x7f {
			return false
		}
	}
	return true
}

// Validate checks a template, its compose file included.
func (t *Template) Validate(ctx context.Context) error {
	switch {
	case !idRE.MatchString(t.ID):
		return errors.New("id: lowercase letters, digits and dashes")
	case t.Name == "" || !printable(t.Name, 60, false):
		return errors.New("name: 1 to 60 characters")
	case !printable(t.Description, 300, false) || !printable(t.Category, 30, false) || !printable(t.Notes, 2000, true):
		return errors.New("description, category or notes too long")
	case t.Website != "" && (!strings.HasPrefix(t.Website, "https://") || !printable(t.Website, 200, false)):
		return errors.New("website: an https:// URL")
	case t.Version == "" || !printable(t.Version, 40, false):
		return errors.New("version: 1 to 40 characters")
	case t.Revision < 1:
		return errors.New("revision: 1 or more")
	case t.MemoryMB < 0:
		return errors.New("memory_mb: not negative")
	}
	keys := map[string]bool{}
	domains := 0
	sample := map[string]string{}
	for _, in := range t.Inputs {
		if !keyRE.MatchString(in.Key) || keys[in.Key] {
			return fmt.Errorf("input %q: a unique UPPER_CASE key", in.Key)
		}
		keys[in.Key] = true
		if in.Label == "" || !printable(in.Label, 60, false) || !printable(in.Help, 300, false) {
			return fmt.Errorf("input %s: label of 1 to 60 characters, help of at most 300", in.Key)
		}
		switch in.Type {
		case InputDomain:
			domains++
			sample[in.Key] = "app.example.com"
		case InputText:
			sample[in.Key] = "text"
		case InputEmail:
			sample[in.Key] = "admin@example.com"
		default:
			return fmt.Errorf("input %s: type must be domain, text or email", in.Key)
		}
		if in.Default != "" {
			if _, err := in.value(in.Default); err != nil {
				return fmt.Errorf("input %s: default: %w", in.Key, err)
			}
			sample[in.Key] = in.Default
		}
	}
	if domains != 1 {
		return errors.New("a template has exactly one domain input")
	}
	for _, sec := range t.Secrets {
		if !keyRE.MatchString(sec.Key) || keys[sec.Key] {
			return fmt.Errorf("secret %q: a unique UPPER_CASE key", sec.Key)
		}
		keys[sec.Key] = true
		if sec.Length < 16 || sec.Length > 128 || (sec.Charset != "" && sec.Charset != "alnum" && sec.Charset != "hex") {
			return fmt.Errorf("secret %s: length 16 to 128, charset alnum or hex", sec.Key)
		}
		sample[sec.Key] = strings.Repeat("s", sec.Length)
	}
	p, err := compose.Parse(ctx, []byte(t.Compose), sample)
	if err != nil {
		return fmt.Errorf("compose file: %w", err)
	}
	if len(p.Warnings) > 0 {
		return fmt.Errorf("compose file: %s", p.Warnings[0])
	}
	if t.Expose.Port < 1 || t.Expose.Port > 65535 {
		return errors.New("expose: a service and its port")
	}
	if err := p.Publish(t.Expose.Service, t.Expose.Port); err != nil {
		return fmt.Errorf("expose: %w", err)
	}
	t.Services = nil
	for _, s := range p.Services {
		if !digest.MatchString(s.Image) || strings.Contains(s.Image, ":latest@") {
			return fmt.Errorf("service %s: pin the image by tag and digest (name:tag@sha256:…), not %q", s.Name, s.Image)
		}
		t.Services = append(t.Services, Service{Name: s.Name, Image: s.Image})
	}
	t.Volumes = p.Volumes
	return nil
}

// value checks an input value.
func (in *Input) value(v string) (string, error) {
	v = strings.TrimSpace(v)
	switch in.Type {
	case InputDomain:
		d, err := proxy.NormalizeDomain(v)
		if err != nil {
			return "", err
		}
		return d, nil
	case InputEmail:
		a, err := mail.ParseAddress(v)
		if err != nil || a.Name != "" || a.Address != v {
			return "", errors.New("not an email address")
		}
	default:
		if !printable(v, 200, false) {
			return "", errors.New("at most 200 characters on one line")
		}
	}
	return v, nil
}

// Install is what installing a template takes: the environment of the
// app (inputs and fresh secrets) and its domain.
type Install struct {
	Env    map[string]string
	Domain string
}

// Prepare validates the user's inputs and generates the secrets.
func (t *Template) Prepare(inputs map[string]string) (*Install, error) {
	for k := range inputs {
		if !slices.ContainsFunc(t.Inputs, func(in Input) bool { return in.Key == k }) {
			return nil, fmt.Errorf("unknown input %s", k)
		}
	}
	out := &Install{Env: map[string]string{}}
	for _, in := range t.Inputs {
		v, ok := inputs[in.Key]
		if !ok || strings.TrimSpace(v) == "" {
			v = in.Default
		}
		if strings.TrimSpace(v) == "" {
			if in.Required || in.Type == InputDomain {
				return nil, fmt.Errorf("%s is required", in.Label)
			}
			out.Env[in.Key] = ""
			continue
		}
		v, err := in.value(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", in.Label, err)
		}
		out.Env[in.Key] = v
		if in.Type == InputDomain {
			out.Domain = v
		}
	}
	for _, sec := range t.Secrets {
		out.Env[sec.Key] = generate(sec.Length, sec.Charset)
	}
	return out, nil
}

// generate makes a random string of n characters.
func generate(n int, charset string) string {
	chars := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if charset == "hex" {
		chars = "0123456789abcdef"
	}
	out := make([]byte, 0, n)
	b := make([]byte, 1)
	// Reject bytes beyond the last whole multiple of the charset size, so
	// every character is equally likely.
	limit := 256 - 256%len(chars)
	for len(out) < n {
		rand.Read(b)
		if int(b[0]) < limit {
			out = append(out, chars[int(b[0])%len(chars)])
		}
	}
	return string(out)
}

// Parse reads a template from its metadata and compose file and
// validates it.
func Parse(ctx context.Context, meta, composeFile []byte) (*Template, error) {
	var t Template
	dec := yaml.NewDecoder(bytes.NewReader(meta))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("template.yaml: %w", err)
	}
	t.Compose = string(composeFile)
	if err := t.Validate(ctx); err != nil {
		return nil, fmt.Errorf("template %s: %w", t.ID, err)
	}
	return &t, nil
}

//go:embed templates
var builtinFS embed.FS

// Builtin returns the templates shipped with Basalt, sorted by name.
var Builtin = sync.OnceValues(func() ([]*Template, error) {
	return Load(context.Background(), builtinFS, "templates")
})

// Load reads every template under dir: one directory per template with
// template.yaml and compose.yaml. The directory is named after the id.
func Load(ctx context.Context, fsys fs.FS, dir string) ([]*Template, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []*Template
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, err := fs.ReadFile(fsys, path.Join(dir, e.Name(), "template.yaml"))
		if err != nil {
			return nil, err
		}
		cf, err := fs.ReadFile(fsys, path.Join(dir, e.Name(), "compose.yaml"))
		if err != nil {
			return nil, err
		}
		t, err := Parse(ctx, meta, cf)
		if err != nil {
			return nil, err
		}
		if t.ID != e.Name() {
			return nil, fmt.Errorf("template %s is in directory %s", t.ID, e.Name())
		}
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b *Template) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out, nil
}

// Find returns the template with the given id.
func Find(list []*Template, id string) *Template {
	i := slices.IndexFunc(list, func(t *Template) bool { return t.ID == id })
	if i < 0 {
		return nil
	}
	return list[i]
}

// Complete returns the environment of an installed app updated to this
// template: its values are kept; inputs the template added take the
// value given (or their default) and secrets it added are generated.
// Added lists the keys added.
func (t *Template) Complete(env, inputs map[string]string) (out map[string]string, added []string, err error) {
	out = maps.Clone(env)
	if out == nil {
		out = map[string]string{}
	}
	for k := range inputs {
		i := slices.IndexFunc(t.Inputs, func(in Input) bool { return in.Key == k })
		if i < 0 {
			return nil, nil, fmt.Errorf("unknown input %s", k)
		}
		if _, ok := env[k]; ok {
			return nil, nil, fmt.Errorf("%s is set already: change it under Environment", t.Inputs[i].Label)
		}
	}
	for _, in := range t.Inputs {
		if _, ok := out[in.Key]; ok {
			continue
		}
		v := inputs[in.Key]
		if strings.TrimSpace(v) == "" {
			v = in.Default
		}
		if strings.TrimSpace(v) == "" {
			if in.Required || in.Type == InputDomain {
				return nil, nil, fmt.Errorf("the update needs %s", in.Label)
			}
		} else if v, err = in.value(v); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", in.Label, err)
		}
		out[in.Key] = v
		added = append(added, in.Key)
	}
	for _, sec := range t.Secrets {
		if _, ok := out[sec.Key]; !ok {
			out[sec.Key] = generate(sec.Length, sec.Charset)
			added = append(added, sec.Key)
		}
	}
	return out, added, nil
}

// Missing returns the inputs an update to this template would ask for:
// those the app's environment lacks and that have no default.
func (t *Template) Missing(env map[string]string) []Input {
	var out []Input
	for _, in := range t.Inputs {
		if _, ok := env[in.Key]; !ok && in.Default == "" {
			out = append(out, in)
		}
	}
	return out
}
