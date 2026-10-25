//go:build !linux

package php

import (
	"os"
	"time"
)

// changeTime: PHP sites run on Linux; elsewhere the modification time.
func changeTime(fi os.FileInfo) time.Time { return fi.ModTime() }
