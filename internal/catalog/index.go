package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

// IndexVersion is the format of catalog indexes this panel reads.
const IndexVersion = 1

// MaxIndexSize bounds a downloaded index.
const MaxIndexSize = 8 << 20

// Index is a published catalog: every template's files in one signed
// file (catalog.json, signed into catalog.json.sig).
type Index struct {
	Version int `json:"version"`
	// Generated orders indexes: a panel refuses one older than it has.
	Generated time.Time    `json:"generated"`
	Templates []IndexEntry `json:"templates"`
}

// IndexEntry holds a template's files as they are in its directory.
type IndexEntry struct {
	Template string `json:"template"` // template.yaml
	Compose  string `json:"compose"`  // compose.yaml
}

// BuildIndex reads the templates under dir (as Load does) and writes the
// index of them.
func BuildIndex(ctx context.Context, fsys fs.FS, dir string, now time.Time) ([]byte, error) {
	if _, err := Load(ctx, fsys, dir); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	idx := Index{Version: IndexVersion, Generated: now.UTC().Truncate(time.Second)}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, _ := fs.ReadFile(fsys, path.Join(dir, e.Name(), "template.yaml"))
		cf, _ := fs.ReadFile(fsys, path.Join(dir, e.Name(), "compose.yaml"))
		idx.Templates = append(idx.Templates, IndexEntry{Template: string(meta), Compose: string(cf)})
	}
	return json.MarshalIndent(idx, "", "  ")
}

// Remote is a verified index: its valid templates, and why the others
// were left out.
type Remote struct {
	Generated time.Time
	Templates []*Template
	Skipped   []string
}

// ParseIndex verifies an index's signature by trusted (an authorized_keys
// line) and reads its templates. A template that does not validate (one
// using a field this panel does not know yet, say) is skipped, not fatal.
func ParseIndex(ctx context.Context, data, sig []byte, trusted string) (*Remote, error) {
	if len(data) > MaxIndexSize {
		return nil, errors.New("the catalog index is too large")
	}
	if err := VerifySSHSig(data, sig, trusted, Namespace); err != nil {
		return nil, fmt.Errorf("catalog signature: %w", err)
	}
	var idx Index
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&idx); err != nil {
		return nil, fmt.Errorf("catalog index: %w", err)
	}
	if idx.Version != IndexVersion {
		return nil, fmt.Errorf("catalog index version %d: this panel reads version %d (update Basalt)", idx.Version, IndexVersion)
	}
	if idx.Generated.IsZero() {
		return nil, errors.New("catalog index without a date")
	}
	r := &Remote{Generated: idx.Generated}
	for i, e := range idx.Templates {
		t, err := Parse(ctx, []byte(e.Template), []byte(e.Compose))
		if err != nil {
			r.Skipped = append(r.Skipped, fmt.Sprintf("#%d: %v", i+1, err))
			continue
		}
		if Find(r.Templates, t.ID) != nil {
			return nil, fmt.Errorf("catalog index lists %s twice", t.ID)
		}
		r.Templates = append(r.Templates, t)
	}
	return r, nil
}

// Merge combines the built-in templates with remote ones: for each id the
// higher revision wins (the built-in one on a tie). Source tells where
// each comes from.
func Merge(builtin, remote []*Template, source string) []*Template {
	out := make([]*Template, 0, len(builtin)+len(remote))
	for _, t := range builtin {
		c := *t
		c.Source = "builtin"
		out = append(out, &c)
	}
	for _, t := range remote {
		c := *t
		c.Source = source
		if i := slices.IndexFunc(out, func(o *Template) bool { return o.ID == t.ID }); i >= 0 {
			if t.Revision > out[i].Revision {
				out[i] = &c
			}
			continue
		}
		out = append(out, &c)
	}
	slices.SortFunc(out, func(a, b *Template) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out
}
