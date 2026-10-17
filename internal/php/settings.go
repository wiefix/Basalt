package php

import (
	"errors"
	"regexp"
)

// Settings are the PHP options a site may change: typed values in fixed
// ranges, never free-form ini lines. Zero means the default.
type Settings struct {
	MemoryMB         int    `json:"memory_mb,omitempty"`          // memory_limit (default 256)
	UploadMB         int    `json:"upload_mb,omitempty"`          // upload_max_filesize and post_max_size (default 64)
	MaxChildren      int    `json:"max_children,omitempty"`       // PHP processes at once (default 10)
	MaxExecutionTime int    `json:"max_execution_time,omitempty"` // seconds (default 30)
	MaxInputVars     int    `json:"max_input_vars,omitempty"`     // default 1000
	DisplayErrors    bool   `json:"display_errors,omitempty"`     // show errors in pages (development)
	Timezone         string `json:"timezone,omitempty"`           // date.timezone, e.g. Europe/Berlin (default UTC)
}

var timezoneRE = regexp.MustCompile(`^(UTC|[A-Z][A-Za-z_]{1,30}(/[A-Za-z0-9_+-]{1,30}){1,2})$`)

// Validate checks the ranges.
func (s Settings) Validate() error {
	in := func(v, lo, hi int) bool { return v == 0 || (v >= lo && v <= hi) }
	switch {
	case !in(s.MemoryMB, 32, 8192):
		return errors.New("memory limit must be 32–8192 MB")
	case !in(s.UploadMB, 1, 4096):
		return errors.New("upload size must be 1–4096 MB")
	case !in(s.MaxChildren, 1, 200):
		return errors.New("PHP processes must be 1–200")
	case !in(s.MaxExecutionTime, 1, 3600):
		return errors.New("execution time must be 1–3600 seconds")
	case !in(s.MaxInputVars, 100, 100000):
		return errors.New("input variables must be 100–100000")
	case s.Timezone != "" && !timezoneRE.MatchString(s.Timezone):
		return errors.New("timezone must be like Europe/Berlin or UTC")
	}
	return nil
}
