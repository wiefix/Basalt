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

func TestRenderSettings(t *testing.T) {
	h := DefaultHost("/run/basalt/php")
	conf, err := h.Render(Pool{SiteID: 7, Version: "8.4", User: "basalt-s7", SocketGroup: "basalt-www", SiteDir: "/srv/basalt/sites/7",
		Settings: Settings{MemoryMB: 512, UploadMB: 200, MaxChildren: 4, MaxExecutionTime: 120, MaxInputVars: 5000, DisplayErrors: true, Timezone: "Europe/Berlin"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"memory_limit] = 512M", "upload_max_filesize] = 200M", "post_max_size] = 200M", "pm.max_children = 4",
		"max_execution_time] = 120", "max_input_vars] = 5000", "date.timezone] = Europe/Berlin",
		"php_flag[display_errors] = on", "request_terminate_timeout = 150s",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("pool lacks %q", want)
		}
	}
	def, _ := h.Render(Pool{SiteID: 7, Version: "8.4", User: "basalt-s7", SocketGroup: "basalt-www", SiteDir: "/srv/basalt/sites/7"})
	for _, want := range []string{"max_execution_time] = 30", "date.timezone] = UTC", "display_errors] = off", "max_input_vars] = 1000"} {
		if !strings.Contains(def, want) {
			t.Errorf("default pool lacks %q", want)
		}
	}
	for i, bad := range []Settings{
		{MemoryMB: 16}, {UploadMB: 5000}, {MaxChildren: 201}, {MaxExecutionTime: -1}, {MaxInputVars: 50},
		{Timezone: "UTC\nphp_admin_value[open_basedir] = /"}, {Timezone: "Europe/Berlin;x"}, {Timezone: "europe/berlin"},
	} {
		if bad.Validate() == nil {
			t.Errorf("case %d valid", i)
		}
		if _, err := h.Render(Pool{SiteID: 7, Version: "8.4", User: "basalt-s7", SocketGroup: "g", SiteDir: "/srv/x", Settings: bad}); err == nil {
			t.Errorf("case %d rendered", i)
		}
	}
	for _, ok := range []string{"UTC", "Europe/Berlin", "America/Argentina/Buenos_Aires", "Etc/GMT+3"} {
		if (Settings{Timezone: ok}).Validate() != nil {
			t.Errorf("%s refused", ok)
		}
	}
}
