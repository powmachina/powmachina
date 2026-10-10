//go:build unix

package wallet

import (
	"errors"
	"os"
	"syscall"
)

// ErrLocked: another process has the wallet open. Two processes over one file would overwrite each
// other's state (the last write wins) — and with money that means forgotten coins.
var ErrLocked = errors.New("wallet: the wallet is already open in another process (wallet-cli or wallet-rpc)")

type fileLock struct{ f *os.File }

func lockFile(path string) (*fileLock, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrLocked
	}
	return &fileLock{f}, nil
}

func (l *fileLock) unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
	l.f = nil
}
