# How to contribute to PowMachina

Thank you. A few things worth knowing before you send a PR.

## Do not send a security bug as a PR

A public PR describing a hole is an attack manual until the networks have it fixed.
Write to **security@powmachina.com**, encrypted if you like; `SECURITY.md` has the details.

## Under what license you submit it

The project is under **BSD 3-clause** (`LICENSE`). By sending a PR you agree that your contribution
goes into the project under that same license.

We do not want to sign any CLA. Instead, **sign your commit with the DCO**
([Developer Certificate of Origin](https://developercertificate.org/)) — one line that
`git commit -s` adds for you:

```
Signed-off-by: First Last <mail@example.com>
```

It says that what you are sending is yours (or that you have the right to send it) and that it may go
under the project's license. Nothing more, you transfer nothing.

## A consensus change has a higher bar

Consensus is `internal/chain`, `internal/mh`, `internal/randomx` and the parameters in `params.go`.
There, a **one-constant difference = a split network**, so:

- **Test vectors or nothing.** A change that changes a hash, a block or validation has to come with
  vectors in the repo and with a test that checks them.
- **One implementation.** A second PoW implementation "just for verification" is not written, not
  even a pure-Go RandomX: the PoW has exactly one implementation, and a one-constant difference
  between two of them is a chain split.
- **No floats in consensus**, explicit little-endian, a defined order.
- The GPU kernel has to be **bit-exact** against `internal/mh` and CI checks that.
- The algorithm's parameters are not changed without calibration (the algorithm spec, §7).

If you are not sure whether your change is consensus, open an issue first and ask. A rewritten PR
hurts less than a split network.

## Ordinary PRs

- `go build ./... && go test ./...` has to pass. Write tests for what you change.
- Comment the **why**, not the what. The code says what it does; a comment should give the reason and
  the trap.
- The commit message: to the point, the first line up to ~70 characters, the body for the "why".
- Stick to the surrounding style — `gofmt`, no new dependencies without a good reason.

## Forking

The license allows forking, commercially and closed-source too. Just keep the copyright notice
(`LICENSE` and `NOTICE`, RandomX included) and do not promote your product with our name or with the
names of the RandomX authors — that is the third clause of BSD-3.
