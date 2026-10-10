package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
)

type obj = map[string]any

// BuildOptions are host-level settings, independent of the routes.
type BuildOptions struct {
	Dir       string // proxy data directory (admin socket, cert storage)
	HTTPPort  int
	HTTPSPort int
	// ACMEDirectory overrides the public CA (e.g. Let's Encrypt staging).
	ACMEDirectory string
}

const zeroSSLDirectory = "https://acme.zerossl.com/v2/DV90"

// App routes take their upstream from a table the agent maintains through
// the admin API (see caddyrun), not from the configuration.
const (
	AppUpstreamSource = "basalt_app"
	AppUpstreamsPath  = "/basalt/app-upstreams"
)

// Files inside the proxy data directory.
func AdminSocket(dir string) string { return filepath.Join(dir, "admin.sock") }
func ConfigPath(dir string) string  { return filepath.Join(dir, "config.json") }
func storagePath(dir string) string { return filepath.Join(dir, "storage") }

// Never served from static sites even if present in the site root. PHP
// sources are hidden too: a site switched from PHP to static must not
// expose its code (and the credentials in it) as plain text.
var hiddenFiles = []string{".git", ".svn", ".hg", ".env", ".htaccess", ".htpasswd", ".DS_Store", ".user.ini", "*.php"}

func adminConfig(dir string) obj {
	return obj{
		"listen": "unix/" + AdminSocket(dir) + "|0600",
		// The agent re-applies desired state; Caddy must not autosave its own copy.
		"config": obj{"persist": false},
	}
}

// BaseConfig is what the proxy runs before the agent has delivered anything:
// only the admin endpoint.
func BaseConfig(dir string) ([]byte, error) {
	return json.Marshal(obj{"admin": adminConfig(dir)})
}

// Build renders the Caddy JSON for d. It returns the config and a stable
// identifier of its content, embedded in the config as an @id so the agent
// can cheaply check what the proxy is running.
func Build(d Desired, o BuildOptions) (cfg []byte, id string, err error) {
	if o.HTTPPort == 0 || o.HTTPSPort == 0 {
		return nil, "", fmt.Errorf("HTTP and HTTPS ports are required")
	}
	var internalNames []string
	var routes []any
	filesystems := []any{}
	for _, r := range d.Routes {
		if r.TLS == TLSInternal {
			internalNames = append(internalNames, r.Domains...)
		} else {
			for _, dom := range r.Domains {
				if !publicCertEligible(dom) {
					internalNames = append(internalNames, dom)
				}
			}
		}
		handlers, fsEntry := routeHandlers(r)
		if fsEntry != nil {
			filesystems = append(filesystems, fsEntry)
		}
		routes = append(routes, obj{
			"match":    []any{obj{"host": r.Domains}},
			"handle":   []any{obj{"handler": "subroute", "routes": []any{obj{"handle": handlers}}}},
			"terminal": true,
		})
	}
	routes = append(routes, obj{"handle": []any{obj{
		"handler": "static_response", "status_code": 404, "body": "Not found\n",
	}}})

	httpsListen := ":" + strconv.Itoa(o.HTTPSPort)
	server := obj{
		"listen":    []any{httpsListen},
		"routes":    routes,
		"protocols": []any{"h1", "h2", "h3"},
	}

	cfgMap := obj{
		"admin": adminConfig(o.Dir),
		"storage": obj{
			"module": "file_system",
			"root":   storagePath(o.Dir),
		},
		"apps": obj{
			"caddy.filesystems": obj{"filesystems": filesystems},
			"http": obj{
				"http_port":    o.HTTPPort,
				"https_port":   o.HTTPSPort,
				"grace_period": "10s",
				"servers":      obj{"basalt": server},
			},
			"tls": obj{"automation": obj{"policies": tlsPolicies(internalNames, d.ACMEEmail, o.ACMEDirectory)}},
			// Never touch the host's trust store with the internal CA root.
			"pki": obj{"certificate_authorities": obj{"local": obj{"install_trust": false}}},
		},
	}

	b, err := json.Marshal(cfgMap)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(b)
	id = "basalt-" + hex.EncodeToString(sum[:12])
	server["@id"] = id
	b, err = json.Marshal(cfgMap)
	return b, id, err
}

func routeHandlers(r Route) (handlers []any, fsEntry obj) {
	set := obj{
		"X-Content-Type-Options": []any{"nosniff"},
		"Referrer-Policy":        []any{"strict-origin-when-cross-origin"},
	}
	if r.HSTS {
		set["Strict-Transport-Security"] = []any{"max-age=31536000"}
	}
	handlers = append(handlers, obj{
		"handler": "headers",
		"response": obj{
			"set":    set,
			"delete": []any{"Server", "X-Powered-By"},
			// Deferred so headers from upstreams are adjusted too.
			"deferred": true,
		},
	})

	switch r.Kind {
	case KindStatic:
		if r.Root == "" || (r.PHP && r.PHPSocket == "") {
			// Unresolved site route: serve nothing rather than guess a path.
			return append(handlers, obj{"handler": "static_response", "status_code": 503}), nil
		}
		fsName := "basalt-route-" + strconv.FormatInt(r.ID, 10)
		fsEntry = obj{
			"name":        fsName,
			"file_system": obj{"backend": "basalt_root", "root": r.Root},
		}
		static := []any{
			obj{
				"handler":   "encode",
				"encodings": obj{"zstd": obj{}, "gzip": obj{}},
				"prefer":    []any{"zstd", "gzip"},
			},
			obj{
				"handler":             "file_server",
				"fs":                  fsName,
				"root":                ".",
				"hide":                hiddenFiles,
				"precompressed":       obj{"zstd": obj{}, "br": obj{}, "gzip": obj{}},
				"precompressed_order": []any{"zstd", "br", "gzip"},
			},
		}
		if !r.PHP {
			return append(handlers, static...), fsEntry
		}
		return append(handlers, obj{"handler": "subroute", "routes": phpRoutes(r, fsName, static)}), fsEntry
	case KindReverseProxy:
		rp := obj{
			"handler": "reverse_proxy",
			// Retry failed connections briefly (an upstream restarting)
			// instead of answering 502 at once.
			"load_balancing": obj{"try_duration": "2s", "try_interval": "250ms"},
		}
		if r.AppID > 0 {
			rp["dynamic_upstreams"] = obj{"source": AppUpstreamSource, "app_id": r.AppID}
		} else {
			rp["upstreams"] = []any{obj{"dial": r.Upstream}}
		}
		handlers = append(handlers, rp)
	}
	return handlers, fsEntry
}

func tlsPolicies(internalNames []string, email, acmeDirectory string) []any {
	var policies []any
	if len(internalNames) > 0 {
		policies = append(policies, obj{
			"subjects": internalNames,
			"issuers":  []any{obj{"module": "internal"}},
		})
	}
	var issuers []any
	if acmeDirectory != "" {
		issuers = []any{acmeIssuer(acmeDirectory, email)}
	} else {
		issuers = []any{acmeIssuer("", email)}
		if email != "" {
			// ZeroSSL needs an email to obtain its account credentials.
			issuers = append(issuers, acmeIssuer(zeroSSLDirectory, email))
		}
	}
	return append(policies, obj{"issuers": issuers})
}

func acmeIssuer(directory, email string) obj {
	iss := obj{"module": "acme"}
	if directory != "" {
		iss["ca"] = directory
	}
	if email != "" {
		iss["email"] = email
	}
	return iss
}

// phpRoutes mirrors Caddy's php_fastcgi directive: redirect directories to
// their canonical slash form when they hold an index.php, rewrite missing
// paths to index.php (front controller), send *.php to PHP-FPM and serve the
// rest as static files. File checks use the confined file system.
func phpRoutes(r Route, fsName string, static []any) []any {
	const path = "{http.request.uri.path}"
	return []any{
		obj{
			"match": []any{obj{
				"file": obj{"fs": fsName, "root": ".", "try_files": []any{path + "/index.php"}},
				"not":  []any{obj{"path": []any{"*/"}}},
			}},
			"handle": []any{obj{
				"handler":     "static_response",
				"status_code": 308,
				"headers":     obj{"Location": []any{"{http.request.orig_uri.path}/{http.request.orig_uri.prefixed_query}"}},
			}},
		},
		obj{
			"match": []any{obj{"file": obj{
				"fs": fsName, "root": ".",
				"try_files":  []any{path, path + "/index.php", "index.php"},
				"try_policy": "first_exist_fallback",
				"split_path": []any{".php"},
			}}},
			"handle": []any{obj{"handler": "rewrite", "uri": "{http.matchers.file.relative}"}},
		},
		obj{
			"match": []any{obj{"path": []any{"*.php"}}},
			"handle": []any{obj{
				"handler": "reverse_proxy",
				"transport": obj{
					"protocol":   "fastcgi",
					"root":       r.Root,
					"split_path": []any{".php"},
				},
				"upstreams": []any{obj{"dial": "unix/" + r.PHPSocket}},
			}},
		},
		obj{"handle": static},
	}
}
