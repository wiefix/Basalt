package e2e

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agent"
)

// pebbleImage is Let's Encrypt's ACME test server.
const pebbleImage = "ghcr.io/letsencrypt/pebble:2.10.1"

// TestWildcardCertificateWithDocker obtains a wildcard certificate from
// Pebble through the DNS-01 challenge: the proxy asks the agent, the panel
// publishes the TXT record, Pebble looks it up on the agent's DNS server.
func TestWildcardCertificateWithDocker(t *testing.T) {
	if os.Getenv("BASALT_DOCKER_TESTS") != "1" {
		t.Skip("set BASALT_DOCKER_TESTS=1 to run against the local Docker daemon")
	}
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	dnsPort := strconv.Itoa(freePort(t))

	// Pebble validates against our DNS server on the host. Pulled first:
	// otherwise the pull's progress (stderr) is mixed into the container ID.
	dockerOut(t, sock, "pull", "-q", pebbleImage)
	id := strings.TrimSpace(dockerOut(t, sock, "run", "-d", "--rm", "-p", "127.0.0.1::14000",
		"--add-host", "host.docker.internal:host-gateway",
		"-e", "PEBBLE_VA_NOSLEEP=1", "-e", "PEBBLE_WFE_NONCEREJECT=0",
		pebbleImage, "-dnsserver", "host.docker.internal:"+dnsPort))
	if len(id) < 12 || strings.ContainsAny(id, " \n") {
		t.Fatalf("start pebble: %s", id)
	}
	t.Cleanup(func() { dockerOut(t, sock, "rm", "-f", id) })
	port := strings.TrimSpace(dockerOut(t, sock, "port", id, "14000/tcp"))
	_, pport, _ := net.SplitHostPort(port)
	caDir := t.TempDir()
	dockerOut(t, sock, "cp", id+":/test/certs/pebble.minica.pem", caDir)
	caFile := filepath.Join(caDir, "pebble.minica.pem")
	if _, err := os.Stat(caFile); err != nil {
		t.Fatal(err)
	}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "wild "+r.Host)
	}))
	defer backend.Close()

	p := startPanel(t)
	h := startHostWith(t, p, "", func(c *agent.Config) {
		c.DNSListen = "0.0.0.0:" + dnsPort
		c.ACMEDirectory = "https://localhost:" + pport + "/dir"
		c.ACMECARoots = []string{caFile}
		c.ACMEResolvers = []string{"127.0.0.1:" + dnsPort}
	})
	p.call(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]string{"name": "example.com"})
	p.call(http.StatusCreated, "POST", "/api/v1/servers/"+h.id+"/routes", map[string]any{
		"kind": "reverse_proxy", "domains": []string{"*.example.com"}, "upstream": strings.TrimPrefix(backend.URL, "http://"), "tls": "auto"})

	// The proxy serves the Pebble-issued wildcard certificate for any name.
	var cert string
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		conn, err := tls.Dial("tcp", "127.0.0.1:"+strconv.Itoa(h.httpsPort), &tls.Config{ServerName: "foo.example.com", InsecureSkipVerify: true})
		if err == nil {
			c := conn.ConnectionState().PeerCertificates[0]
			conn.Close()
			if slices.Contains(c.DNSNames, "*.example.com") && strings.Contains(c.Issuer.CommonName, "Pebble") {
				cert = c.Issuer.CommonName
				break
			}
		}
		time.Sleep(time.Second)
	}
	if cert == "" {
		t.Fatalf("no wildcard certificate from Pebble; pebble log:\n%s", dockerOut(t, sock, "logs", id))
	}
	t.Logf("certificate for *.example.com issued by %q", cert)
	for _, name := range []string{"foo.example.com", "bar.example.com"} {
		if code, body := h.get(name, "/"); code != 200 || body != "wild "+name {
			t.Fatalf("%s: %d %q", name, code, body)
		}
	}
	// The challenge record is gone once the certificate is issued.
	eventually(t, "challenge removed", func() bool {
		var n int
		p.db.R.QueryRow(`SELECT COUNT(*) FROM dns_challenges`).Scan(&n)
		return n == 0
	})
}
