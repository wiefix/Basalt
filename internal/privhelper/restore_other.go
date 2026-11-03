//go:build !linux

package privhelper

// exchange swaps folders a and b through spare (servers run Linux, where
// the swap is at once).
func exchange(a, b, spare string) error {
	return renameSwap(a, b, spare)
}

// dropXattrs does nothing: servers run Linux.
func dropXattrs(string) error { return nil }
