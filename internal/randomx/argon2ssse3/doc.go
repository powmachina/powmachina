// Package argon2ssse3 carries a single source file: the Argon2 variant for SSSE3.
//
// It is a separate package because cgo applies CFLAGS to a whole package. If `-mssse3` were set
// globally, the compiler would be free to emit SSSE3 anywhere and the binary would crash on a CPU that
// does not have it. This way the switch is confined to one file.
//
// Which path actually gets used is decided by RandomX at runtime based on the CPU
// (RANDOMX_FLAG_ARGON2_*). On architectures without SSSE3 an empty version is compiled that returns
// NULL — the way upstream does it.
package argon2ssse3

/*
#cgo CFLAGS: -O3 -w -I${SRCDIR}/../csrc/randomx/src -include ${SRCDIR}/../mh_salt.h
*/
import "C"
