package server

import "testing"

func TestCSVCell(t *testing.T) {
	for in, want := range map[string]string{
		"":              "",
		"app.created":   "app.created",
		"=HYPERLINK(1)": "'=HYPERLINK(1)",
		"+1":            "'+1",
		"-1":            "'-1",
		"@SUM(A1)":      "'@SUM(A1)",
		"\tx":           "'\tx",
		"a=b":           "a=b",
		"192.0.2.1":     "192.0.2.1",
		`{"name":"=x"}`: `{"name":"=x"}`,
	} {
		if got := csvCell(in); got != want {
			t.Errorf("csvCell(%q) = %q, want %q", in, got, want)
		}
	}
}
