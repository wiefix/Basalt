package rules

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// pathFuncs are the os functions that resolve a path themselves, following
// symlinks in every component (and in the last one, but for L* and
// Remove*). Inside a folder its user controls, the user can swap a
// component for a symlink between a check and the use: the agent, root,
// then works on a file anywhere. os.Root methods resolve beneath the root.
var pathFuncs = []string{
	"Chmod", "Chown", "Chtimes", "Create", "CreateTemp", "DirFS", "Lchown", "Link", "Lstat",
	"Mkdir", "MkdirAll", "MkdirTemp", "Open", "OpenFile", "ReadDir", "ReadFile", "Readlink",
	"Remove", "RemoveAll", "Rename", "Stat", "Symlink", "Truncate", "WriteFile",
}

// walkFuncs are the path/filepath functions that touch the disk.
var walkFuncs = []string{"EvalSymlinks", "Glob", "Walk", "WalkDir"}

// rootOnly are the packages that work on paths users choose (the file
// manager, SFTP): every file operation goes through os.Root, and a path is
// joined with path.Join after files.Clean, never filepath.Join.
var rootOnly = []string{"internal/files", "internal/sftpd", "internal/siteworker"}

// siteMethods and siteFields give a site's folder (or the folder of all
// sites) in the agent: sitesManager.dir and PublicDir, sitesManager.root
// and siteWatcher.root. (checkout.root is an *os.Root, which no path
// function takes.)
var (
	siteMethods = []string{"dir", "PublicDir"}
	siteFields  = []string{"root"}
)

// sitePathsAllowed are the path-based calls checkSitePaths lets through, as
// "package: function call", with the reason. The sites folder and the trash
// in it belong to root (0755, 0700): a site's user cannot put a symlink
// there, only below its own folder.
var sitePathsAllowed = map[string]string{
	"internal/agent: sitesManager.siteDir os.Lstat":                 "the site's own folder, in the root-owned sites folder; the worker opens it as the site's user",
	"internal/sftpd: LoadOrCreateHostKey os.ReadFile":               "the host key in the agent's data folder, not a site's",
	"internal/sftpd: LoadOrCreateHostKey os.OpenFile":               "the host key in the agent's data folder, not a site's",
	"internal/agent: inotifyBackend.addTreeLocked filepath.WalkDir": "only adds inotify watches (IN_DONT_FOLLOW | IN_ONLYDIR), read nothing and change nothing",
}

// checkSitePaths finds path-based file operations where a user can plant
// symlinks: anywhere in rootOnly, and in the agent on paths made from a
// site's folder (traced through assignments and the parameters of the
// package's functions). allowed lists the exceptions; one that matches
// nothing any more is reported too.
func checkSitePaths(pkgs map[string]*pkg, allowed map[string]string) []violation {
	var out []violation
	used := map[string]bool{}
	sink := func(p *pkg, fn string, call *ast.CallExpr, name string) {
		key := p.path + ": " + fn + " " + name
		if _, ok := allowed[key]; ok {
			used[key] = true
			return
		}
		msg := name + " in " + fn + " resolves a path a site's user controls: use os.Root (or add \"" + key + "\" to sitePathsAllowed with the reason)"
		out = append(out, violation{p.fset.Position(call.Pos()), msg})
	}
	for _, p := range sortedPkgs(pkgs) {
		switch {
		case slices.Contains(rootOnly, p.path):
			for _, f := range p.files {
				osName, fpName := importName(f, "os"), importName(f, "path/filepath")
				for _, fd := range funcDecls(f) {
					ast.Inspect(fd, func(n ast.Node) bool {
						if call, ok := n.(*ast.CallExpr); ok {
							if name := pathCall(call, osName, fpName, true); name != "" {
								sink(p, funcName(fd), call, name)
							}
						}
						return true
					})
				}
			}
		case p.path == "internal/agent":
			t := newTaint(p)
			t.run()
			for _, f := range p.files {
				osName, fpName := importName(f, "os"), importName(f, "path/filepath")
				for _, fd := range funcDecls(f) {
					ast.Inspect(fd, func(n ast.Node) bool {
						call, ok := n.(*ast.CallExpr)
						if !ok {
							return true
						}
						if name := pathCall(call, osName, fpName, false); name != "" && slices.ContainsFunc(call.Args, func(a ast.Expr) bool { return t.tainted(fd, a) }) {
							sink(p, funcName(fd), call, name)
						}
						return true
					})
				}
			}
		}
	}
	for _, key := range sortedKeys(allowed) {
		pkgPath, _, _ := strings.Cut(key, ": ")
		if pkgs[pkgPath] != nil && !used[key] {
			out = append(out, violation{token.Position{Filename: "internal/rules/paths_test.go"}, "sitePathsAllowed: \"" + key + "\" matches nothing: remove it"})
		}
	}
	return out
}

// pathCall names a path-based file operation ("os.Lstat"), or returns "".
// join also counts filepath.Join.
func pathCall(call *ast.CallExpr, osName, fpName string, join bool) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	switch {
	case id.Name == osName && slices.Contains(pathFuncs, sel.Sel.Name):
		return "os." + sel.Sel.Name
	case id.Name == fpName && (slices.Contains(walkFuncs, sel.Sel.Name) || join && sel.Sel.Name == "Join"):
		return "filepath." + sel.Sel.Name
	}
	return ""
}

// taint traces which names in the agent's functions hold a site's path.
// Names are per function (closures included), without scopes: shadowing
// would only make it see more.
type taint struct {
	p     *pkg
	funcs map[string][]*ast.FuncDecl // by name: methods of different types merge
	names map[*ast.FuncDecl]map[string]bool
	pkgs  map[*ast.FuncDecl]map[string]bool // imported package names in the file
}

func newTaint(p *pkg) *taint {
	t := &taint{p: p, funcs: map[string][]*ast.FuncDecl{}, names: map[*ast.FuncDecl]map[string]bool{}, pkgs: map[*ast.FuncDecl]map[string]bool{}}
	for _, f := range p.files {
		imported := map[string]bool{}
		for _, imp := range f.Imports {
			ip, _ := strconv.Unquote(imp.Path.Value)
			name := path.Base(ip)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			imported[name] = true
		}
		for _, fd := range funcDecls(f) {
			t.funcs[fd.Name.Name] = append(t.funcs[fd.Name.Name], fd)
			t.names[fd] = map[string]bool{}
			t.pkgs[fd] = imported
		}
	}
	return t
}

// run spreads the taint until nothing changes.
func (t *taint) run() {
	for changed := true; changed; {
		changed = false
		mark := func(fd *ast.FuncDecl, name string) {
			if name != "_" && !t.names[fd][name] {
				t.names[fd][name] = true
				changed = true
			}
		}
		for _, fds := range t.funcs {
			for _, fd := range fds {
				ast.Inspect(fd, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.AssignStmt:
						if slices.ContainsFunc(n.Rhs, func(e ast.Expr) bool { return t.tainted(fd, e) }) {
							for _, l := range n.Lhs {
								if id, ok := l.(*ast.Ident); ok {
									mark(fd, id.Name)
								}
							}
						}
					case *ast.ValueSpec:
						if slices.ContainsFunc(n.Values, func(e ast.Expr) bool { return t.tainted(fd, e) }) {
							for _, id := range n.Names {
								mark(fd, id.Name)
							}
						}
					case *ast.RangeStmt:
						if t.tainted(fd, n.X) {
							for _, e := range []ast.Expr{n.Key, n.Value} {
								if id, ok := e.(*ast.Ident); ok {
									mark(fd, id.Name)
								}
							}
						}
					case *ast.CallExpr:
						if !slices.ContainsFunc(n.Args, func(e ast.Expr) bool { return t.tainted(fd, e) }) {
							return true
						}
						// A callback gets paths from the call (WalkDir).
						for _, a := range n.Args {
							if lit, ok := a.(*ast.FuncLit); ok {
								for _, name := range paramNames(lit.Type) {
									mark(fd, name)
								}
							}
						}
						for _, callee := range t.callees(fd, n) {
							params := paramNames(callee.Type)
							for i, a := range n.Args {
								if len(params) > 0 && t.tainted(fd, a) {
									mark(callee, params[min(i, len(params)-1)])
								}
							}
						}
					}
					return true
				})
			}
		}
	}
}

// callees are the package's functions a call may reach, by name.
func (t *taint) callees(fd *ast.FuncDecl, call *ast.CallExpr) []*ast.FuncDecl {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return t.funcs[fun.Name]
	case *ast.SelectorExpr:
		if id, ok := fun.X.(*ast.Ident); ok && t.pkgs[fd][id.Name] {
			return nil
		}
		return t.funcs[fun.Sel.Name]
	}
	return nil
}

// tainted reports whether e is made from a site's path in fd.
func (t *taint) tainted(fd *ast.FuncDecl, e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		switch n := n.(type) {
		case *ast.Ident:
			found = t.names[fd][n.Name]
		case *ast.SelectorExpr:
			if id, ok := n.X.(*ast.Ident); ok && t.pkgs[fd][id.Name] {
				return false // a package's name, not a field
			}
			found = slices.Contains(siteFields, n.Sel.Name)
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && slices.Contains(siteMethods, sel.Sel.Name) {
				if id, ok := sel.X.(*ast.Ident); !ok || !t.pkgs[fd][id.Name] {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// paramNames are the names of the parameters, "_" for those that cannot
// hold a path (not a string or a list of them).
func paramNames(ft *ast.FuncType) []string {
	var out []string
	for _, f := range ft.Params.List {
		typ := f.Type
		switch t := typ.(type) {
		case *ast.Ellipsis:
			typ = t.Elt
		case *ast.ArrayType:
			typ = t.Elt
		}
		id, ok := typ.(*ast.Ident)
		str := ok && id.Name == "string"
		if len(f.Names) == 0 {
			out = append(out, "_")
		}
		for _, n := range f.Names {
			if str {
				out = append(out, n.Name)
			} else {
				out = append(out, "_")
			}
		}
	}
	return out
}

func funcDecls(f *ast.File) []*ast.FuncDecl {
	var out []*ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
			out = append(out, fd)
		}
	}
	return out
}

// funcName is "Type.Method" or "Func".
func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if g, ok := t.(*ast.IndexExpr); ok {
		t = g.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// importName is the name a file uses for the import path, or "" without
// that import.
func importName(f *ast.File, importPath string) string {
	for _, imp := range f.Imports {
		if imp.Path.Value != strconv.Quote(importPath) {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return path.Base(importPath)
	}
	return ""
}

// symlinkServers serve a folder through a file system that follows
// symlinks out of it: a site's symlink would serve another site's files
// (the proxy reads every site). Allowed per package, with the reason.
var (
	symlinkServers = []string{"os.DirFS", "http.Dir", "http.ServeFile", "http.FileServer"}
	symlinkAllowed = map[string]string{"cmd/basalt os.DirFS": "the catalog command reads the operator's own folder"}
)

// checkStaticFiles finds Caddy file_server handlers without the "fs" of
// caddy.fs.basalt_root (os.Root: symlinks stay inside the site's public
// folder), and file systems that follow symlinks out of their folder.
func checkStaticFiles(pkgs map[string]*pkg) []violation {
	var out []violation
	for _, p := range sortedPkgs(pkgs) {
		for _, f := range p.files {
			names := map[string]string{importName(f, "os"): "os", importName(f, "net/http"): "http"}
			delete(names, "")
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					id, ok := n.X.(*ast.Ident)
					if !ok || names[id.Name] == "" {
						return true
					}
					name := names[id.Name] + "." + n.Sel.Name
					if slices.Contains(symlinkServers, name) {
						if _, ok := symlinkAllowed[p.path+" "+name]; !ok {
							out = append(out, violation{p.fset.Position(n.Pos()), name + " follows symlinks out of its folder: serve files through os.Root"})
						}
					}
				case *ast.CompositeLit:
					keys := map[string]string{}
					for _, e := range n.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok {
							keys[stringLit(kv.Key)] = stringLit(kv.Value)
						}
					}
					if _, fs := keys["fs"]; keys["handler"] == "file_server" && !fs {
						out = append(out, violation{p.fset.Position(n.Pos()), "Caddy file_server without \"fs\": serve through caddy.fs.basalt_root, Caddy's own file system follows symlinks"})
					}
				}
				return true
			})
		}
	}
	return out
}

func TestSitePaths(t *testing.T) { report(t, checkSitePaths(repo(t), sitePathsAllowed)) }

func TestStaticFiles(t *testing.T) { report(t, checkStaticFiles(repo(t))) }

// TestRulesCatchSitePaths makes sure the site path and static file checks
// find what they are meant to find.
func TestRulesCatchSitePaths(t *testing.T) {
	src := map[string]string{
		"internal/agent": `package agent
import (
	"os"
	fp "path/filepath"
)
func (m *sitesManager) fix(id int64) {
	d, _ := m.dir(id)
	pub := fp.Join(d, "public")
	os.Chmod(pub, 0o750)
	m.own(pub)
	os.ReadFile("/etc/basalt/agent.json")
	fp.WalkDir(d, func(p string, _ any, _ error) error { return os.Lchown(p, 0, 0) })
}
func (m *sitesManager) own(path string) { os.Lchown(path, 1, 1) }
func (m *sitesManager) list() { os.ReadDir(m.root) }
`,
		"internal/files": `package files
import (
	"os"
	"path/filepath"
)
func Open(dir string) { os.OpenRoot(dir); os.Stat(filepath.Join(dir, "x")) }
`,
		"internal/proxy": `package proxy
import web "net/http"
type obj map[string]any
var _ = obj{"handler": "file_server", "root": "/srv"}
var _ = obj{"handler": "file_server", "fs": "basalt-route-1"}
var _ = web.FileServer(web.Dir("/srv"))
`,
	}
	pkgs := parsePkgs(t, src)
	got := msgs(checkSitePaths(pkgs, map[string]string{
		"internal/agent: sitesManager.list os.ReadDir": "root-owned",
		"internal/agent: sitesManager.gone os.Remove":  "stale",
	}))
	want := []string{
		"0:sitePathsAllowed: \"internal/agent: sitesManager.gone os.Remove\" matches nothing",
		"12:filepath.WalkDir in sitesManager.fix",
		"12:os.Lchown in sitesManager.fix",
		"14:os.Lchown in sitesManager.own",
		"6:filepath.Join in Open",
		"6:os.Stat in Open",
		"9:os.Chmod in sitesManager.fix",
	}
	if !matches(got, want) {
		t.Errorf("site paths: got %q, want %q", got, want)
	}
	got = msgs(checkStaticFiles(pkgs))
	want = []string{"4:Caddy file_server without \"fs\"", "6:http.Dir follows", "6:http.FileServer follows"}
	if !matches(got, want) {
		t.Errorf("static files: got %q, want %q", got, want)
	}
}

func parsePkgs(t *testing.T, src map[string]string) map[string]*pkg {
	t.Helper()
	fset := token.NewFileSet()
	pkgs := map[string]*pkg{}
	for path, s := range src {
		f, err := parser.ParseFile(fset, path+"/x.go", s, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		pkgs[path] = &pkg{path: path, imports: map[string][]token.Position{}, files: []*ast.File{f}, fset: fset}
	}
	return pkgs
}

func msgs(vs []violation) []string {
	var out []string
	for _, v := range vs {
		out = append(out, fmt.Sprintf("%d:%s", v.pos.Line, v.msg))
	}
	slices.Sort(out)
	return out
}

func matches(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			return false
		}
	}
	return true
}
