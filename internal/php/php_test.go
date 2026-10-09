package php

import (
	"strings"
	"testing"
)

func TestRenderPool(t *testing.T) {
	h := DefaultHost("/run/basalt/php")
	conf, err := h.Render(Pool{SiteID: 7, Version: "8.4", User: "basalt-s7", SocketGroup: "basalt-www", SiteDir: "/srv/basalt/sites/7"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[basalt-site-7]", "user = basalt-s7", "listen = /run/basalt/php/site-7.sock",
		"listen.group = basalt-www", "listen.mode = 0660",
		"php_admin_value[open_basedir] = /srv/basalt/sites/7/",
		"php_admin_value[session.save_path] = /srv/basalt/sites/7/tmp",
		"shell_exec", "pm.max_children = 10", "memory_limit] = 256M",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("pool lacks %q", want)
		}
	}
}

func TestRenderRejectsInjection(t *testing.T) {
	h := DefaultHost("/run/basalt/php")
	bad := []Pool{
		{SiteID: 1, Version: "8.4\nuser=root", User: "basalt-s1", SocketGroup: "g", SiteDir: "/srv/x"},
		{SiteID: 1, Version: "8.4", User: "basalt-s1\n[evil]", SocketGroup: "g", SiteDir: "/srv/x"},
		{SiteID: 1, Version: "8.4", User: "basalt-s1", SocketGroup: "g", SiteDir: "/srv/x;rm"},
		{SiteID: 1, Version: "8.4", User: "basalt-s1", SocketGroup: "g", SiteDir: "relative"},
		{SiteID: 0, Version: "8.4", User: "basalt-s1", SocketGroup: "g", SiteDir: "/srv/x"},
	}
	for i, p := range bad {
		if _, err := h.Render(p); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}
