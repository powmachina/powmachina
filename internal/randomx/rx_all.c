// The C part of RandomX. The Argon2 variants for SSSE3/AVX2 are not here — they need their own
// compiler switches, and in cgo those cannot be applied to a single file. They therefore live in
// their own packages (argon2ssse3/, argon2avx2/).
#include "csrc/randomx/src/argon2_core.c"
#include "csrc/randomx/src/argon2_ref.c"
#include "csrc/randomx/src/blake2/blake2b.c"
#include "csrc/randomx/src/reciprocal.c"
#include "csrc/randomx/src/virtual_memory.c"
