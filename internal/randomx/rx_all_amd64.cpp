// The RandomX translation unit (x86-64).
//
// cgo only compiles files DIRECTLY in the package directory, and the vendored sources have to stay
// untouched (csrc/VENDOR-RANDOMX.md). That is why they are pulled in here with `#include`:
// one shim instead of twenty thin wrappers.
//
// That merging them into one unit changed nothing is held by randomx_conformance_test.go —
// it runs upstream's official test vectors against this build.
#include "csrc/randomx/src/aes_hash.cpp"
#include "csrc/randomx/src/allocator.cpp"
#include "csrc/randomx/src/assembly_generator_x86.cpp"
#include "csrc/randomx/src/blake2_generator.cpp"
#include "csrc/randomx/src/bytecode_machine.cpp"
#include "csrc/randomx/src/cpu.cpp"
#include "csrc/randomx/src/dataset.cpp"
#include "csrc/randomx/src/instruction.cpp"
#include "csrc/randomx/src/instructions_portable.cpp"
#include "csrc/randomx/src/jit_compiler_x86.cpp"
#include "csrc/randomx/src/randomx.cpp"
#include "csrc/randomx/src/soft_aes.cpp"
#include "csrc/randomx/src/superscalar.cpp"
#include "csrc/randomx/src/virtual_machine.cpp"
#include "csrc/randomx/src/vm_compiled.cpp"
#include "csrc/randomx/src/vm_compiled_light.cpp"
#include "csrc/randomx/src/vm_interpreted.cpp"
#include "csrc/randomx/src/vm_interpreted_light.cpp"
