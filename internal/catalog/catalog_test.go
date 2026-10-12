package catalog

import (
	"context"
	"strings"
	"testing"
)

func TestBuiltin(t *testing.T) {
	list, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) < 6 {
		t.Fatalf("%d templates", len(list))
	}
	for _, tpl := range list {
		if len(tpl.Services) == 0 || tpl.Expose.Service == "" || tpl.Notes == "" {
			t.Errorf("%s: %+v", tpl.ID, tpl)
		}
	}
	if Find(list, "wordpress") == nil || Find(list, "nope") != nil {
		t.Fatal("find")
	}
}

const okMeta = `
id: demo
name: Demo
version: "1.0"
revision: 1
inputs:
  - {key: DOMAIN, label: Domain, type: domain, required: true}
  - {key: ADMIN_EMAIL, label: Admin email, type: email, default: admin@example.com}
  - {key: TITLE, label: Title, type: text}
secrets:
  - {key: PASS, length: 20}
  - {key: KEY, length: 32, charset: hex}
expose: {service: web, port: 80}
notes: hi
`

const okCompose = `
services:
  web:
    image: nginx:1.29@sha256:0000000000000000000000000000000000000000000000000000000000000000
    environment:
      HOST: ${DOMAIN}
      MAIL: ${ADMIN_EMAIL}
      TITLE: ${TITLE}
      PASS: ${PASS}
      KEY: ${KEY}
    volumes: [data:/data]
volumes:
  data:
`

func TestParse(t *testing.T) {
	ctx := context.Background()
	tpl, err := Parse(ctx, []byte(okMeta), []byte(okCompose))
	if err != nil {
		t.Fatal(err)
	}
	if tpl.Services[0].Name != "web" || tpl.Volumes[0] != "data" {
		t.Fatalf("%+v", tpl)
	}
	for name, c := range map[string]struct{ meta, compose string }{
		"latest":         {okMeta, strings.Replace(okCompose, "nginx:1.29@", "nginx:latest@", 1)},
		"no digest":      {okMeta, strings.Replace(okCompose, "@sha256:0000000000000000000000000000000000000000000000000000000000000000", "", 1)},
		"digest only":    {okMeta, strings.Replace(okCompose, "nginx:1.29@", "nginx@", 1)},
		"undeclared":     {okMeta, strings.Replace(okCompose, "${TITLE}", "${OTHER}", 1)},
		"privileged":     {okMeta, strings.Replace(okCompose, "    volumes: [data:/data]", "    privileged: true\n    volumes: [data:/data]", 1)},
		"bind mount":     {okMeta, strings.Replace(okCompose, "data:/data", "/etc:/data", 1)},
		"unknown field":  {okMeta + "extra: 1\n", okCompose},
		"no domain":      {strings.Replace(okMeta, "type: domain", "type: text", 1), okCompose},
		"bad input type": {strings.Replace(okMeta, "type: email", "type: number", 1), okCompose},
		"short secret":   {strings.Replace(okMeta, "length: 20", "length: 8", 1), okCompose},
		"dup key":        {strings.Replace(okMeta, "key: KEY", "key: PASS", 1), okCompose},
		"lower key":      {strings.Replace(okMeta, "key: TITLE", "key: title", 1), strings.Replace(okCompose, "${TITLE}", "${title}", 1)},
		"bad default":    {strings.Replace(okMeta, "default: admin@example.com", "default: not-an-email", 1), okCompose},
		"expose missing": {strings.Replace(okMeta, "service: web", "service: api", 1), okCompose},
		"no revision":    {strings.Replace(okMeta, "revision: 1", "revision: 0", 1), okCompose},
		"bad id":         {strings.Replace(okMeta, "id: demo", "id: Demo!", 1), okCompose},
		"http website":   {okMeta + "website: http://example.com\n", okCompose},
		"multiline name": {strings.Replace(okMeta, "name: Demo", `name: "a\nb"`, 1), okCompose},
		"no compose":     {okMeta, ""},
		"container_name": {okMeta, strings.Replace(okCompose, "    volumes: [data:/data]", "    container_name: x\n    volumes: [data:/data]", 1)},
		"published port": {okMeta, strings.Replace(okCompose, "    volumes: [data:/data]", "    ports: [\"80:80\"]\n    volumes: [data:/data]", 1)},
	} {
		if _, err := Parse(ctx, []byte(c.meta), []byte(c.compose)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPrepare(t *testing.T) {
	tpl, err := Parse(context.Background(), []byte(okMeta), []byte(okCompose))
	if err != nil {
		t.Fatal(err)
	}
	in, err := tpl.Prepare(map[string]string{"DOMAIN": " Blog.Example.COM ", "TITLE": "My blog"})
	if err != nil {
		t.Fatal(err)
	}
	if in.Domain != "blog.example.com" || in.Env["DOMAIN"] != "blog.example.com" || in.Env["ADMIN_EMAIL"] != "admin@example.com" ||
		in.Env["TITLE"] != "My blog" || len(in.Env["PASS"]) != 20 || len(in.Env["KEY"]) != 32 {
		t.Fatalf("%+v", in)
	}
	if strings.Trim(in.Env["KEY"], "0123456789abcdef") != "" || strings.Trim(in.Env["PASS"], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
		t.Fatalf("charset: %q %q", in.Env["KEY"], in.Env["PASS"])
	}
	again, _ := tpl.Prepare(map[string]string{"DOMAIN": "blog.example.com"})
	if again.Env["PASS"] == in.Env["PASS"] {
		t.Fatal("secrets repeat")
	}
	for name, inputs := range map[string]map[string]string{
		"no domain":  {},
		"bad domain": {"DOMAIN": "not a domain"},
		"wildcard":   {"DOMAIN": "*.example.com"},
		"bad email":  {"DOMAIN": "a.example.com", "ADMIN_EMAIL": "Admin <a@example.com>"},
		"newline":    {"DOMAIN": "a.example.com", "TITLE": "a\nb"},
		"unknown":    {"DOMAIN": "a.example.com", "PASS": "mine"},
		"long":       {"DOMAIN": "a.example.com", "TITLE": strings.Repeat("x", 201)},
	} {
		if _, err := tpl.Prepare(inputs); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestComplete(t *testing.T) {
	meta := strings.Replace(okMeta, "secrets:", "  - {key: OWNER, label: Owner, type: text, required: true}\nsecrets:", 1)
	tpl, err := Parse(context.Background(), []byte(meta), []byte(strings.Replace(okCompose, "TITLE: ${TITLE}", "TITLE: ${TITLE}${OWNER}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	old := map[string]string{"DOMAIN": "a.example.com", "PASS": "kept-password-123456", "CUSTOM": "mine"}
	if m := tpl.Missing(old); len(m) != 2 || m[0].Key != "TITLE" || m[1].Key != "OWNER" {
		t.Fatalf("missing %+v", m)
	}
	if _, _, err := tpl.Complete(old, nil); err == nil || !strings.Contains(err.Error(), "Owner") {
		t.Fatalf("required input: %v", err)
	}
	if _, _, err := tpl.Complete(old, map[string]string{"OWNER": "x", "DOMAIN": "b.example.com"}); err == nil {
		t.Fatal("existing value replaced")
	}
	env, added, err := tpl.Complete(old, map[string]string{"OWNER": "boss"})
	if err != nil {
		t.Fatal(err)
	}
	if env["PASS"] != old["PASS"] || env["CUSTOM"] != "mine" || env["OWNER"] != "boss" || env["ADMIN_EMAIL"] != "admin@example.com" ||
		env["TITLE"] != "" || len(env["KEY"]) != 32 || strings.Join(added, ",") != "ADMIN_EMAIL,TITLE,OWNER,KEY" {
		t.Fatalf("env %v added %v", env, added)
	}
	if old["OWNER"] != "" {
		t.Fatal("the old environment changed")
	}
}
