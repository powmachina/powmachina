//go:build !unix && !windows

package wallet

import "errors"

// ErrLocked is not used here — there is no file lock outside unix and Windows (a web wallet has no file).
var ErrLocked = errors.New("wallet: the wallet is already open in another process")

type fileLock struct{}

func lockFile(string) (*fileLock, error) { return &fileLock{}, nil }
func (l *fileLock) unlock()              {}
