package randomx

/*
// On ARM the cryptographic extensions (AES among them) come from -march=armv8-a+crypto.
// The equivalent of -maes on x86; without it HAVE_AES is zero and lane C is several times slower.
#cgo CFLAGS:   -march=armv8-a+crypto
#cgo CXXFLAGS: -march=armv8-a+crypto
// RandomX asks the kernel whether the CPU has AES (getauxval, cpu.cpp) only under HAVE_HWCAP, which
// its CMake defines and cgo does not: without it a Linux arm64 node ran lane C without hardware AES
// on CPUs that have it. The result is the same either way, only slower.
#cgo linux CFLAGS:   -DHAVE_HWCAP
#cgo linux CXXFLAGS: -DHAVE_HWCAP
*/
import "C"
