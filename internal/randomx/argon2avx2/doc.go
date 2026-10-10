// Package argon2avx2 carries a single source file: the Argon2 variant for AVX2.
//
// It is a separate package because cgo applies CFLAGS to a whole package. If `-mavx2` were set
// globally, the compiler would be free to emit AVX2 anywhere and the binary would crash on a CPU that
// does not have it. This way the switch is confined to one file.
//
// Which path actually gets used is decided by RandomX at runtime based on the CPU
// (RANDOMX_FLAG_ARGON2_*). On architectures without AVX2 an empty version is compiled that returns
// NULL — the way upstream does it.
package argon2avx2

/*
#cgo CFLAGS: -O3 -w -I${SRCDIR}/../csrc/randomx/src -include ${SRCDIR}/../mh_salt.h
*/
import "C"
