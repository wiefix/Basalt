package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/wiefix/basalt/internal/catalog"
)

func TestCatalogBuild(t *testing.T) {
	var out bytes.Buffer
	if err := runCatalog(context.Background(), []string{"build", "../../internal/catalog/templates"}, &out); err != nil {
		t.Fatal(err)
	}
	var idx catalog.Index
	if err := json.Unmarshal(out.Bytes(), &idx); err != nil || idx.Version != catalog.IndexVersion || len(idx.Templates) < 6 {
		t.Fatalf("%v %+v", err, idx)
	}
	for _, args := range [][]string{{}, {"build"}, {"sign", "x"}, {"build", t.TempDir() + "/missing"}} {
		if err := runCatalog(context.Background(), args, &out); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}
