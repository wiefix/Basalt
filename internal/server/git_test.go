package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
)

func TestVerifyWebhook(t *testing.T) {
	secret := []byte("0123456789abcdef")
	body := []byte(`{"ref":"refs/heads/main"}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	for name, c := range map[string]struct {
		h     http.Header
		event string
		ok    bool
	}{
		"github":       {http.Header{"X-Hub-Signature-256": {"sha256=" + sig}, "X-Github-Event": {"push"}}, "push", true},
		"gitea":        {http.Header{"X-Gitea-Signature": {sig}, "X-Gitea-Event": {"push"}}, "push", true},
		"gogs":         {http.Header{"X-Gogs-Signature": {sig}, "X-Gogs-Event": {"push"}}, "push", true},
		"gitlab":       {http.Header{"X-Gitlab-Token": {string(secret)}, "X-Gitlab-Event": {"Push Hook"}}, "push hook", true},
		"github bad":   {http.Header{"X-Hub-Signature-256": {"sha256=" + sig[:63] + "0"}, "X-Github-Event": {"push"}}, "push", false},
		"gitlab bad":   {http.Header{"X-Gitlab-Token": {"guess"}}, "", false},
		"unsigned":     {http.Header{"X-Github-Event": {"push"}}, "push", false},
		"wrong prefix": {http.Header{"X-Hub-Signature-256": {"sha1=" + sig}}, "", false},
	} {
		event, ok := verifyWebhook(c.h, body, secret)
		if ok != c.ok || (c.event != "" && event != c.event) {
			t.Errorf("%s: event %q ok %v; want %q %v", name, event, ok, c.event, c.ok)
		}
	}
	// A signature over another body fails.
	if _, ok := verifyWebhook(http.Header{"X-Hub-Signature-256": {"sha256=" + sig}}, []byte(`{}`), secret); ok {
		t.Error("signature accepted for a different body")
	}
}
