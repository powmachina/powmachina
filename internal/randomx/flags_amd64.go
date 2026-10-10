package randomx

/*
// Without -maes HAVE_AES is zero (intrin_portable.h only defines it under __AES__),
// randomx_get_flags() does not return HARD_AES and lane C is several times slower. This only compiles
// the hardware AES code in — whether it gets used is decided by a runtime flag based on the CPU.
#cgo CFLAGS:   -maes
#cgo CXXFLAGS: -maes
*/
import "C"
