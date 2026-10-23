//go:build !unix

package standby

func lockDir(string) (func(), error) { return func() {}, nil }

func matchOwner(string) error { return nil }
