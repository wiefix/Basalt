package agenthub

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDeadlineWriterSurvivesIdle is a regression test: the write deadline
// used to stay armed after a write, and HTTP/2 resets a stream whose
// deadline passes even while idle, so quiet agent channels were dropped.
func TestDeadlineWriterSurvivesIdle(t *testing.T) {
	const timeout = 100 * time.Millisecond
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dw := &deadlineWriter{w: w, rc: http.NewResponseController(w), timeout: timeout}
		dw.Write([]byte("a"))
		time.Sleep(4 * timeout) // idle longer than the deadline
		dw.Write([]byte("b"))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("protocol %s; the test needs HTTP/2", resp.Proto)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil || string(b) != "ab" {
		t.Fatalf("read %q, %v; the idle stream was reset", b, err)
	}
}
