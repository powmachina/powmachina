/* The Argon2d salt for MachinaHash-C.
 *
 * It is pulled in through `-include`, so EVERY RandomX translation unit sees it.
 * `configuration.h` is patched to `#ifndef`, so ours wins (see
 * csrc/randomx/powmachina.patch and csrc/VENDOR-RANDOMX.md).
 *
 * With `-DMH_RANDOMX_STOCK_SALT=1` (build tag `mh_stock_salt`) the upstream salt is kept —
 * then the official RandomX test vectors can be run to verify that our build is
 * conformant. That is what randomx_conformance_test.go is for.
 */
#ifndef MH_SALT_H
#define MH_SALT_H

#ifndef MH_RANDOMX_STOCK_SALT
#define RANDOMX_ARGON_SALT "MachinaHash-C\x01"
#endif

#endif /* MH_SALT_H */
