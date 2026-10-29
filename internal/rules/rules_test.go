package rules

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// pkg is one package of the repository, without its tests: test code may
// import anything.
type pkg struct {
	path    string // relative to the module, e.g. "internal/agent"
	imports map[string][]token.Position
	files   []*ast.File
	fset    *token.FileSet
}

// repo parses every non-test Go file of the module once.
func repo(t *testing.T) map[string]*pkg {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	module := modulePath(t, filepath.Join(root, "go.mod"))
	fset := token.NewFileSet()
	pkgs := map[string]*pkg{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "testdata" || name == "dist") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		if ignored(f) {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		rel = filepath.ToSlash(rel)
		p := pkgs[rel]
		if p == nil {
			p = &pkg{path: rel, imports: map[string][]token.Position{}, fset: fset}
			pkgs[rel] = p
		}
		p.files = append(p.files, f)
		for _, imp := range f.Imports {
			ip, _ := strconv.Unquote(imp.Path.Value)
			if ip == module {
				ip = "."
			} else if after, ok := strings.CutPrefix(ip, module+"/"); ok {
				ip = after
			} else {
				continue
			}
			p.imports[ip] = append(p.imports[ip], fset.Position(imp.Pos()))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pkgs["internal/agent"] == nil || pkgs["internal/server"] == nil {
		t.Fatalf("repository not found from %s", root)
	}
	return pkgs
}

func modulePath(t *testing.T, gomod string) string {
	f, err := os.Open(gomod)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m, ok := strings.CutPrefix(sc.Text(), "module "); ok {
			return strings.TrimSpace(m)
		}
	}
	t.Fatal("no module line in go.mod")
	return ""
}

// ignored reports a file excluded from every build (//go:build ignore).
func ignored(f *ast.File) bool {
	for _, cg := range f.Comments {
		if cg.Pos() > f.Package {
			break
		}
		for _, c := range cg.List {
			if strings.TrimSpace(c.Text) == "//go:build ignore" {
				return true
			}
		}
	}
	return false
}

// panelOnly is code of the control plane. The agent runs as root on every
// server and the proxy faces the internet: neither carries the panel's
// database, secrets or HTTP API, even unused.
var panelOnly = []string{
	"internal/server", "internal/store", "internal/agenthub", "internal/standby",
	"internal/walship", "internal/secrets", "internal/catalog", "internal/compose",
	"internal/dbengine", "internal/github", "internal/forge", "internal/notify",
	"internal/webauthn", "web",
}

// hostSide are the packages that run outside the panel process.
var hostSide = []string{"internal/agent", "internal/proxy/caddyrun"}

// importers lists who may import a package at all: the top of the program
// is wired together only in main, and the panel's database is used only by
// the panel.
var importers = map[string][]string{
	"internal/server":   {"cmd/basalt"},
	"internal/agent":    {"cmd/basalt"},
	"internal/agenthub": {"cmd/basalt"},
	"internal/standby":  {"cmd/basalt", "internal/server"},
	"internal/store":    {"cmd/basalt", "internal/server", "internal/agenthub", "internal/standby"},
}

// testHelper matches packages made for tests (githubtest, webauthntest, ...).
var testHelper = regexp.MustCompile(`[a-z]test$`)

type violation struct {
	pos token.Position
	msg string
}

func checkImports(pkgs map[string]*pkg) []violation {
	var out []violation
	for _, from := range hostSide {
		// Breadth-first, remembering the chain to show how a package got in.
		via := map[string]string{from: ""}
		queue := []string{from}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			p := pkgs[cur]
			if p == nil {
				continue
			}
			for _, imp := range sortedKeys(p.imports) {
				if _, seen := via[imp]; seen {
					continue
				}
				via[imp] = cur
				if slices.Contains(panelOnly, imp) {
					chain := []string{imp}
					for c := cur; c != ""; c = via[c] {
						chain = append([]string{c}, chain...)
					}
					out = append(out, violation{p.imports[imp][0],
						fmt.Sprintf("%s pulls in panel code %s (%s)", from, imp, strings.Join(chain, " → "))})
					continue
				}
				queue = append(queue, imp)
			}
		}
	}
	for _, p := range sortedPkgs(pkgs) {
		for _, imp := range sortedKeys(p.imports) {
			if allowed, ok := importers[imp]; ok && !slices.Contains(allowed, p.path) {
				out = append(out, violation{p.imports[imp][0],
					fmt.Sprintf("%s imports %s (only %s may)", p.path, imp, strings.Join(allowed, ", "))})
			}
			if testHelper.MatchString(imp) {
				out = append(out, violation{p.imports[imp][0],
					fmt.Sprintf("%s imports the test helper %s outside tests", p.path, imp)})
			}
		}
	}
	return out
}

// shells are names a "-c" script would run under.
var shells = []string{"sh", "bash", "dash", "ash", "zsh", "/bin/sh", "/bin/bash", "/bin/dash", "/bin/ash", "/usr/bin/env"}

// shellC is "-c" with other single-letter flags (-ec, -euc).
var shellC = regexp.MustCompile(`^-[a-z]*c[a-z]*$`)

// checkShell finds `sh -c SCRIPT` in argument lists (exec.Command, docker
// exec, an entrypoint) whose SCRIPT is not a constant. Data goes after the
// script as its own argument ($0, $1), never into the script, where a quote
// in it would become shell syntax.
func checkShell(pkgs map[string]*pkg) []violation {
	var out []violation
	for _, p := range sortedPkgs(pkgs) {
		consts := constNames(p.files)
		for _, f := range p.files {
			ast.Inspect(f, func(n ast.Node) bool {
				var list []ast.Expr
				switch n := n.(type) {
				case *ast.CallExpr:
					list = n.Args
				case *ast.CompositeLit:
					list = n.Elts
				default:
					return true
				}
				for i := 0; i+1 < len(list); i++ {
					if !slices.Contains(shells, stringLit(list[i])) || !shellC.MatchString(stringLit(list[i+1])) {
						continue
					}
					pos := p.fset.Position(list[i].Pos())
					if i+2 >= len(list) {
						out = append(out, violation{pos, "shell -c without its script in the same argument list"})
					} else if !constant(list[i+2], consts) {
						out = append(out, violation{pos, "shell -c script is not a constant: pass data as arguments ($0, $1)"})
					}
				}
				return true
			})
		}
	}
	return out
}

func stringLit(e ast.Expr) string {
	if l, ok := e.(*ast.BasicLit); ok && l.Kind == token.STRING {
		s, err := strconv.Unquote(l.Value)
		if err == nil {
			return s
		}
	}
	return "\x00"
}

// constNames are the names declared const anywhere in the package. Shadowing
// a constant by a variable would fool this; nobody does that.
func constNames(files []*ast.File) map[string]bool {
	names := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if d, ok := n.(*ast.GenDecl); ok && d.Tok == token.CONST {
				for _, s := range d.Specs {
					for _, id := range s.(*ast.ValueSpec).Names {
						names[id.Name] = true
					}
				}
			}
			return true
		})
	}
	return names
}

func constant(e ast.Expr, consts map[string]bool) bool {
	switch e := e.(type) {
	case *ast.BasicLit:
		return e.Kind == token.STRING
	case *ast.Ident:
		return consts[e.Name]
	case *ast.ParenExpr:
		return constant(e.X, consts)
	case *ast.BinaryExpr:
		return e.Op == token.ADD && constant(e.X, consts) && constant(e.Y, consts)
	}
	return false
}

// checkSetgid finds octal literals with the setgid bit: in an os.FileMode
// that bit is fs.ModeSetgid, and 0o2000 silently means something else.
func checkSetgid(pkgs map[string]*pkg) []violation {
	var out []violation
	for _, p := range sortedPkgs(pkgs) {
		for _, f := range p.files {
			ast.Inspect(f, func(n ast.Node) bool {
				l, ok := n.(*ast.BasicLit)
				if !ok || l.Kind != token.INT || !octal(l.Value) {
					return true
				}
				if v, err := strconv.ParseUint(strings.ReplaceAll(l.Value, "_", ""), 0, 64); err == nil && v <= 0o7777 && v&0o2000 != 0 {
					out = append(out, violation{p.fset.Position(l.Pos()), "setgid as an octal literal: use fs.ModeSetgid"})
				}
				return true
			})
		}
	}
	return out
}

func octal(lit string) bool {
	return strings.HasPrefix(lit, "0o") || strings.HasPrefix(lit, "0O") ||
		(len(lit) > 1 && lit[0] == '0' && lit[1] >= '0' && lit[1] <= '7')
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func sortedPkgs(pkgs map[string]*pkg) []*pkg {
	var out []*pkg
	for _, k := range sortedKeys(pkgs) {
		out = append(out, pkgs[k])
	}
	return out
}

func report(t *testing.T, vs []violation) {
	t.Helper()
	for _, v := range vs {
		t.Errorf("%s: %s", v.pos, v.msg)
	}
}

func TestImports(t *testing.T) { report(t, checkImports(repo(t))) }

func TestShellScripts(t *testing.T) { report(t, checkShell(repo(t))) }

func TestSetgidLiterals(t *testing.T) { report(t, checkSetgid(repo(t))) }

// TestRulesCatch makes sure each check finds what it is meant to find.
func TestRulesCatch(t *testing.T) {
	src := map[string]string{
		"internal/agent": `package agent
import (
	"os/exec"
	"github.com/wiefix/basalt/internal/files"
	"github.com/wiefix/basalt/internal/github/githubtest"
)
const fixed = "echo ok"
func f(name string) {
	exec.Command("sh", "-c", "rm -rf " + name)
	exec.Command("sh", "-c", fixed, name)
	_ = []string{"/bin/bash", "-ec", fixed + " && " + "true"}
	_ = []string{"sh", "-c"}
	_ = 0o2755
	_ = 02775
	_ = 0o755
}
`,
		"internal/files": `package files
import "github.com/wiefix/basalt/internal/secrets"
`,
		"internal/dns": `package dns
import "github.com/wiefix/basalt/internal/server"
`,
	}
	fset := token.NewFileSet()
	pkgs := map[string]*pkg{}
	for path, s := range src {
		f, err := parser.ParseFile(fset, path+"/x.go", s, 0)
		if err != nil {
			t.Fatal(err)
		}
		p := &pkg{path: path, imports: map[string][]token.Position{}, files: []*ast.File{f}, fset: fset}
		for _, imp := range f.Imports {
			ip, _ := strconv.Unquote(imp.Path.Value)
			p.imports[strings.TrimPrefix(ip, "github.com/wiefix/basalt/")] = []token.Position{fset.Position(imp.Pos())}
		}
		pkgs[path] = p
	}
	msgs := func(vs []violation) []string {
		var out []string
		for _, v := range vs {
			out = append(out, fmt.Sprintf("%d:%s", v.pos.Line, v.msg))
		}
		slices.Sort(out)
		return out
	}
	check := func(name string, got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("%s: got %q, want %d findings", name, got, len(want))
			return
		}
		for i := range want {
			if !strings.Contains(got[i], want[i]) {
				t.Errorf("%s: finding %q does not contain %q", name, got[i], want[i])
			}
		}
	}
	check("imports", msgs(checkImports(pkgs)),
		"2:internal/agent pulls in panel code internal/secrets (internal/agent → internal/files → internal/secrets)",
		"2:internal/dns imports internal/server",
		"5:internal/agent imports the test helper")
	check("shell", msgs(checkShell(pkgs)),
		"12:shell -c without its script",
		"9:shell -c script is not a constant")
	check("setgid", msgs(checkSetgid(pkgs)), "13:setgid", "14:setgid")
}
