//go:build !linux

package agent

// Watching folders needs inotify: elsewhere the panel's round copies the
// sites.
func newWatchBackend(*siteWatcher) watchBackend { return nil }
