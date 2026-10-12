package walletrpc

import (
	"encoding/json"
	"testing"
	"time"
)

// fakeClock replaces the clock of the password limit for one test.
func fakeClock(t *testing.T) *time.Time {
	t.Helper()
	at := time.Unix(1_800_000_000, 0)
	old := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = old })
	return &at
}

// The RPC token alone must not be enough to search for the wallet password: after a few wrong ones in a
// row, no password is looked at for a while — the right one included — and the while grows.
func TestWrongPassphrasesAreSlowedDown(t *testing.T) {
	b := newExchange(t, true)
	clock := fakeClock(t)
	const path = "/wallet/deposit"
	var to string
	json.Unmarshal(b.ok("/wallet/hot", "getnewaddress"), &to)

	wrong := func(why string) {
		t.Helper()
		if r := b.call(path, "walletpassphrase", "guess", 60); r.Error == nil || r.Error.Code != codeWrongPassphrase {
			t.Fatalf("%s: a wrong password answered %+v", why, r.Error)
		}
	}
	held := func(why string) {
		t.Helper()
		r := b.call(path, "walletpassphrase", "pass-dep", 60)
		if r.Error == nil || r.Error.Code != codeWalletError {
			t.Fatalf("%s: the right password was looked at during the wait: %+v", why, r.Error)
		}
		if r := b.call(path, "sendtoaddress", to, 1); r.Error == nil || r.Error.Code != codeUnlockNeeded {
			t.Fatalf("%s: the wallet signs although it was never unlocked: %+v", why, r.Error)
		}
	}

	for range freePassAttempts {
		wrong("within the free attempts")
	}
	held("right after the last free attempt")
	*clock = clock.Add(999 * time.Millisecond)
	held("just before the first wait is over")

	// The wait doubles with every further wrong password.
	*clock = clock.Add(time.Millisecond)
	wrong("after the first wait")
	*clock = clock.Add(1500 * time.Millisecond)
	held("in the middle of the second wait")
	*clock = clock.Add(500 * time.Millisecond)
	wrong("after the second wait")
	*clock = clock.Add(3999 * time.Millisecond)
	held("just before the third wait is over")

	// The other ways of trying a password share the limit …
	if r := b.call(path, "walletpassphrasechange", "pass-dep", "new"); r.Error == nil || r.Error.Code != codeWalletError {
		t.Fatalf("walletpassphrasechange went around the wait: %+v", r.Error)
	}
	// … and another wallet is not affected by it.
	b.ok("/wallet/hot", "walletpassphrase", "pass-hot", 60)

	// The right password after the wait clears the count: the free attempts are back.
	*clock = clock.Add(time.Millisecond)
	b.ok(path, "walletpassphrase", "pass-dep", 60)
	b.ok(path, "sendtoaddress", to, 1)
	for range freePassAttempts - 1 {
		wrong("after a successful unlock")
	}
	b.ok(path, "walletpassphrase", "pass-dep", 60)
}

// However many wrong passwords there were, the wait stops growing at maxPassDelay.
func TestPassphraseWaitHasACeiling(t *testing.T) {
	for fails, want := range map[int]time.Duration{0: 0, freePassAttempts - 1: 0, freePassAttempts: time.Second,
		freePassAttempts + 1: 2 * time.Second, freePassAttempts + 5: 32 * time.Second,
		freePassAttempts + 6: maxPassDelay, 1 << 30: maxPassDelay} {
		if got := passDelay(fails); got != want {
			t.Fatalf("the wait after %d wrong passwords is %v, expected %v", fails, got, want)
		}
	}
}

// With --allow-manage the words are behind the same password, so guessing through getseed is limited too.
func TestGetSeedSharesThePassphraseLimit(t *testing.T) {
	b := newExchange(t, true)
	fakeClock(t)
	b.s.AllowManage = true
	for range freePassAttempts {
		if r := b.call("/", "getseed", "deposit", "guess"); r.Error == nil || r.Error.Code != codeWrongPassphrase {
			t.Fatalf("getseed with a wrong password: %+v", r.Error)
		}
	}
	if r := b.call("/", "getseed", "deposit", "pass-dep"); r.Error == nil || r.Error.Code != codeWalletError {
		t.Fatalf("getseed looked at a password during the wait: %+v", r.Error)
	}
}
