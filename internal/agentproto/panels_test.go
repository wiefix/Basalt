package agentproto

import "testing"

func TestValidPanelURL(t *testing.T) {
	for _, u := range []string{"https://panel.example.com:8443", "https://192.0.2.1:8443", "https://[2001:db8::1]:8443", "https://panel.example.com"} {
		if err := ValidPanelURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range []string{"", "http://panel:8443", "https://", "https://u@panel:8443", "https://panel:8443/x", "https://panel:8443?a", "https://panel:x", "panel:8443"} {
		if ValidPanelURL(u) == nil {
			t.Errorf("%q accepted", u)
		}
	}
}
