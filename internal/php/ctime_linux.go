package php

import (
	"os"
	"syscall"
	"time"
)

// changeTime is when the file's inode last changed (for an installed file:
// when it was put in place).
func changeTime(fi os.FileInfo) time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Ctim.Sec, st.Ctim.Nsec)
	}
	return fi.ModTime()
}
