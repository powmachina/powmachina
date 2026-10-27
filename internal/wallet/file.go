package wallet

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// The wallet.dat file — one binary file, encrypted as a whole with the password.
//
//	magic      8 B   "POWMWLT\x00"
//	version    2 B   the file format (1)
//	network    4 B   the network magic (mainnet, testnet and regtest cannot be mixed up)
//	kdf        1 B   1 = Argon2id (RFC 9106)
//	memory     4 B   KiB
//	time       4 B   the number of passes
//	threads    1 B
//	salt      16 B
//	aead       1 B   1 = XChaCha20-Poly1305 (RFC 8439, 24 B nonce)
//	nonce     24 B
//	length     4 B   the length of the encrypted content
//	sealed     …     the content + a 16 B tag
//
// Every number is little-endian. **The whole header is AAD**: changing the parameters, the network or
// the version breaks the tag just as changing the content does. The content is not parsed until the tag
// matches.
//
// Nothing home-made in the cryptography: Argon2id and XChaCha20-Poly1305 come from
// golang.org/x/crypto. What is ours is only the envelope and its bounds (the parameter maxima protect
// against a file that would eat all the memory when opened).

var fileMagic = [8]byte{'P', 'O', 'W', 'M', 'W', 'L', 'T', 0}

const (
	fileVersion = 1
	kdfArgon2id = 1
	aeadXChaCha = 1
	saltBytes   = 16
	headerBytes = 8 + 2 + 4 + 1 + 4 + 4 + 1 + saltBytes + 1 + chacha20poly1305.NonceSizeX + 4

	// The parameter bounds when opening a file. This build only ever writes DefaultKDF (256 MiB, 3, 4),
	// so the ceilings sit a few multiples above that — enough headroom for a deliberately stronger
	// wallet, low enough that a hostile file cannot make `open` spend gigabytes and minutes in Argon2id
	// (a 4 GiB / 32-pass header used to pass the cheap parse and hang or OOM the opener). A file asking
	// for more than this is refused with ErrBadParams before any key is derived.
	minMemoryKiB = 8 << 10 // 8 MiB
	maxMemoryKiB = 1 << 20 // 1 GiB
	maxTime      = 10
	maxThreads   = 8
	maxSealed    = 1 << 30 // 1 GiB
	maxFileBytes = headerBytes + maxSealed
)

// KDFParams are the Argon2id parameters.
type KDFParams struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
}

// DefaultKDF: 256 MiB, 3 passes, 4 threads — unlocking takes ~1 s on an ordinary PC, and an attack on
// the password with graphics cards is orders of magnitude more expensive than with PBKDF2 (the memory
// cannot be shared between attempts).
var DefaultKDF = KDFParams{MemoryKiB: 256 << 10, Time: 3, Threads: 4}

var (
	ErrNotWallet     = errors.New("wallet: not a PowMachina wallet file")
	ErrFileVersion   = errors.New("wallet: the file is from a newer version of the software")
	ErrWrongNetwork  = errors.New("wallet: the wallet belongs to a different network")
	ErrWrongPassword = errors.New("wallet: wrong password or damaged file")
	ErrBadParams     = errors.New("wallet: encryption parameters are out of allowed bounds")
	ErrNoPassword    = errors.New("wallet: password must not be empty")
)

// sealedFile is the parsed file.
type sealedFile struct {
	network [4]byte
	kdf     KDFParams
	salt    [saltBytes]byte
	nonce   [chacha20poly1305.NonceSizeX]byte
	sealed  []byte
}

func (f *sealedFile) header() []byte {
	var b bytes.Buffer
	b.Write(fileMagic[:])
	_ = binary.Write(&b, binary.LittleEndian, uint16(fileVersion))
	b.Write(f.network[:])
	b.WriteByte(kdfArgon2id)
	_ = binary.Write(&b, binary.LittleEndian, f.kdf.MemoryKiB)
	_ = binary.Write(&b, binary.LittleEndian, f.kdf.Time)
	b.WriteByte(f.kdf.Threads)
	b.Write(f.salt[:])
	b.WriteByte(aeadXChaCha)
	b.Write(f.nonce[:])
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(f.sealed)))
	return b.Bytes()
}

func parseFile(data []byte) (*sealedFile, error) {
	if len(data) < headerBytes || !bytes.Equal(data[:8], fileMagic[:]) {
		return nil, ErrNotWallet
	}
	le := binary.LittleEndian
	f := &sealedFile{}
	p := 8
	if v := le.Uint16(data[p:]); v != fileVersion {
		if v > fileVersion {
			return nil, ErrFileVersion
		}
		return nil, ErrNotWallet
	}
	p += 2
	copy(f.network[:], data[p:])
	p += 4
	if data[p] != kdfArgon2id {
		return nil, ErrNotWallet
	}
	p++
	f.kdf.MemoryKiB = le.Uint32(data[p:])
	f.kdf.Time = le.Uint32(data[p+4:])
	f.kdf.Threads = data[p+8]
	p += 9
	copy(f.salt[:], data[p:])
	p += saltBytes
	if data[p] != aeadXChaCha {
		return nil, ErrNotWallet
	}
	p++
	copy(f.nonce[:], data[p:])
	p += len(f.nonce)
	n := le.Uint32(data[p:])
	p += 4
	if n > maxSealed || int(n) != len(data)-p {
		return nil, ErrNotWallet
	}
	f.sealed = data[p:]
	if err := f.kdf.check(); err != nil {
		return nil, err
	}
	return f, nil
}

func (k KDFParams) check() error {
	if k.MemoryKiB < minMemoryKiB || k.MemoryKiB > maxMemoryKiB || k.Time < 1 || k.Time > maxTime ||
		k.Threads < 1 || k.Threads > maxThreads {
		return ErrBadParams
	}
	return nil
}

// deriveKey is Argon2id(password, salt) → a 32 B key for XChaCha20-Poly1305.
func deriveKey(password string, salt []byte, k KDFParams) []byte {
	return argon2.IDKey([]byte(password), salt, k.Time, k.MemoryKiB, k.Threads, chacha20poly1305.KeySize)
}

// seal encrypts the content with a new nonce. The key and the salt do not change between writes (Argon2
// would cost a second per write); the nonce is 24 random bytes, so a collision is not a risk even after
// billions of writes.
func seal(network [4]byte, kdf KDFParams, salt [saltBytes]byte, key, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	f := &sealedFile{network: network, kdf: kdf, salt: salt}
	if _, err := rand.Read(f.nonce[:]); err != nil {
		return nil, err
	}
	// The length is in the header, and the header is AAD — so it has to be known in advance.
	f.sealed = make([]byte, len(plaintext)+aead.Overhead())
	hdr := f.header()
	aead.Seal(f.sealed[:0], f.nonce[:], plaintext, hdr)
	return append(hdr, f.sealed...), nil
}

// open verifies and decrypts. A wrong password and a changed byte anywhere give the same error.
func (f *sealedFile) open(key []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	out, err := aead.Open(nil, f.nonce[:], f.sealed, f.header())
	if err != nil {
		return nil, ErrWrongPassword
	}
	return out, nil
}

// writeAtomic writes the file so that after a crash either the old or the new one remains — never a half-written one.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // after a successful rename it no longer exists
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

func newSalt() ([saltBytes]byte, error) {
	var s [saltBytes]byte
	_, err := rand.Read(s[:])
	return s, err
}

func netErr(want, got [4]byte) error {
	return fmt.Errorf("%w (file %q, software %q)", ErrWrongNetwork, got[:], want[:])
}
