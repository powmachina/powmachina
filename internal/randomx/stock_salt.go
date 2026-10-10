//go:build mh_stock_salt

package randomx

/*
// With the stock salt our build behaves like upstream RandomX, so its own test vectors can be run
// against it (randomx_conformance_test.go). A normal build runs the PowMachina salt and those vectors
// would — correctly — not match.
#cgo CFLAGS:   -DMH_RANDOMX_STOCK_SALT=1
#cgo CXXFLAGS: -DMH_RANDOMX_STOCK_SALT=1
*/
import "C"
