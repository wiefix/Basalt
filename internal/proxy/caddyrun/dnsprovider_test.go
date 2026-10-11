package caddyrun

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libdns/libdns"
)

func TestDNSProvider(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "bpdns")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "acme.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var c ChallengeRequest
		json.NewDecoder(r.Body).Decode(&c)
		if c.Name == "_acme-challenge.refused.example.com" {
			http.Error(w, "refused.example.com is not a domain of this server", http.StatusBadGateway)
			return
		}
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path+" "+c.Name+" "+c.Value)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	p := &DNSProvider{Socket: sock}
	ctx := context.Background()
	rec := libdns.TXT{Name: "_acme-challenge", TTL: time.Minute, Text: `token-"x"`}
	if _, err := p.AppendRecords(ctx, "example.com.", []libdns.Record{rec}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DeleteRecords(ctx, "example.com.", []libdns.Record{rec}); err != nil {
		t.Fatal(err)
	}
	want := `POST /acme-challenge _acme-challenge.example.com token-"x"|DELETE /acme-challenge _acme-challenge.example.com token-"x"`
	if strings.Join(got, "|") != want {
		t.Fatalf("got %q", got)
	}
	// The agent's reason reaches the certificate log.
	_, err = p.AppendRecords(ctx, "example.com.", []libdns.Record{libdns.TXT{Name: "_acme-challenge.refused", Text: "x"}})
	if err == nil || !strings.Contains(err.Error(), "not a domain of this server") {
		t.Fatalf("refusal: %v", err)
	}
	if _, err := p.AppendRecords(ctx, "example.com.", []libdns.Record{libdns.Address{Name: "x"}}); err == nil {
		t.Fatal("non-TXT record accepted")
	}
	if _, err := (&DNSProvider{Socket: filepath.Join(dir, "missing.sock")}).AppendRecords(ctx, "example.com.", []libdns.Record{rec}); err == nil {
		t.Fatal("missing agent not reported")
	}
}
