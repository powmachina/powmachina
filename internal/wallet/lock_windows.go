//go:build windows

package wallet

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// ErrLocked: another process has the wallet open. Two processes over one file would overwrite each
// other's state (the last write wins) — and with money that means forgotten coins. On Windows that is
// two copies of the desktop wallet, or the wallet and wallet-cli at once.
var ErrLocked = errors.New("wallet: the wallet is already open in another process (the wallet app, wallet-cli or wallet-rpc)")

type fileLock struct{ f *os.File }

// lockFile takes an exclusive lock on `<path>.lock` and fails at once when somebody holds it. The lock
// belongs to the handle, so the system drops it when the process dies — a crash leaves nothing to clean up.
func lockFile(path string) (*fileLock, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, new(windows.Overlapped))
	if err != nil {
		f.Close()
		return nil, ErrLocked
	}
	return &fileLock{f}, nil
}

func (l *fileLock) unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, new(windows.Overlapped))
	l.f.Close()
	l.f = nil
}
