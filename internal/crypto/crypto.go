package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	// Argon2id parameters (RFC 9106 balanced interactive/sensitive desktop profile)
	ArgonTime    uint32 = 3
	ArgonMemory  uint32 = 64 * 1024 // 64 MiB
	ArgonThreads uint8  = 4
	KeySize             = 32 // 256-bit keys
	SaltSize            = 32
	NonceSize           = 12 // AES-GCM standard nonce
	PQMagic             = "YTPQ"
	PQSaltSize          = 16
)

var (
	ErrDecryptFailed = errors.New("falha na descriptografia: senha/arquivo-chave incorretos ou dados corrompidos")
)

// KeyBundle holds the encrypted DEK wrapped by the password(+optional keyfile)-derived KEK.
// Changing the master password or keyfile only re-wraps DEK without re-encrypting videos.
type KeyBundle struct {
	KDFSalt      []byte `json:"kdf_salt"`
	ArgonTime    uint32 `json:"argon_time"`
	ArgonMemory  uint32 `json:"argon_memory"`
	ArgonThreads uint8  `json:"argon_threads"`
	WrappedDEK   []byte `json:"wrapped_dek"` // legacy AES-GCM or YTPQ cascaded
	KeyCheckHash string `json:"key_check_hash"`
	UsesKeyfile  bool   `json:"uses_keyfile,omitempty"`
	KeyfileHint  string `json:"keyfile_hint,omitempty"`
}

// RandomBytes generates cryptographically secure random bytes.
func RandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, fmt.Errorf("csprng error: %w", err)
	}
	return b, nil
}

// RandomHex returns a random hex string of nBytes bytes (2*nBytes hex chars).
func RandomHex(nBytes int) (string, error) {
	b, err := RandomBytes(nBytes)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// GenerateKeyfile creates a high-entropy 512-bit (64-byte) local keyfile on disk.
func GenerateKeyfile(path string) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	raw, err := RandomBytes(64)
	if err != nil {
		return nil, err
	}
	// Encode with readable header + hex so users recognize it safely as a YTCloud keyfile
	content := []byte("YTCLOUD-KEYFILE-V1\n" + hex.EncodeToString(raw) + "\n")
	ZeroBytes(raw)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return nil, err
	}
	return content, nil
}

// DeriveKEK derives a 32-byte Key Encryption Key from password + salt using Argon2id.
func DeriveKEK(password string, salt []byte, timeCost, memKiB uint32, threads uint8) []byte {
	return DeriveKEKWithKeyfile(password, nil, salt, timeCost, memKiB, threads)
}

// DeriveKEKWithKeyfile combines password + optional keyfile SHA-512 hash before Argon2id KDF.
func DeriveKEKWithKeyfile(password string, keyfileBytes []byte, salt []byte, timeCost, memKiB uint32, threads uint8) []byte {
	if timeCost == 0 {
		timeCost = ArgonTime
	}
	if memKiB == 0 {
		memKiB = ArgonMemory
	}
	if threads == 0 {
		threads = ArgonThreads
	}
	secret := []byte(password)
	if len(keyfileBytes) > 0 {
		kfHash := sha512.Sum512(keyfileBytes)
		combined := make([]byte, 0, len(secret)+1+len(kfHash))
		combined = append(combined, secret...)
		combined = append(combined, 0x00)
		combined = append(combined, kfHash[:]...)
		defer ZeroBytes(combined)
		return argon2.IDKey(combined, salt, timeCost, memKiB, threads, KeySize)
	}
	return argon2.IDKey(secret, salt, timeCost, memKiB, threads, KeySize)
}

// CreateKeyBundle creates a new random 256-bit DEK and wraps it with the password.
func CreateKeyBundle(password string) (*KeyBundle, []byte, error) {
	return CreateKeyBundleWithKeyfile(password, nil, "")
}

// CreateKeyBundleWithKeyfile creates a new 256-bit DEK wrapped using password + optional local keyfile (PQ cascaded).
func CreateKeyBundleWithKeyfile(password string, keyfileBytes []byte, keyfileHint string) (*KeyBundle, []byte, error) {
	if len(password) < 4 {
		return nil, nil, errors.New("a senha deve ter pelo menos 4 caracteres")
	}
	salt, err := RandomBytes(SaltSize)
	if err != nil {
		return nil, nil, err
	}
	dek, err := RandomBytes(KeySize)
	if err != nil {
		return nil, nil, err
	}
	kek := DeriveKEKWithKeyfile(password, keyfileBytes, salt, ArgonTime, ArgonMemory, ArgonThreads)
	defer ZeroBytes(kek)

	wrapped, err := EncryptPQ(kek, dek, []byte("YTCloud-DEK-v1"))
	if err != nil {
		ZeroBytes(dek)
		return nil, nil, err
	}
	check := sha256.Sum256(append([]byte("YTCloud-Check-v1:"), dek...))
	return &KeyBundle{
		KDFSalt:      salt,
		ArgonTime:    ArgonTime,
		ArgonMemory:  ArgonMemory,
		ArgonThreads: ArgonThreads,
		WrappedDEK:   wrapped,
		KeyCheckHash: hex.EncodeToString(check[:8]),
		UsesKeyfile:  len(keyfileBytes) > 0,
		KeyfileHint:  keyfileHint,
	}, dek, nil
}

// UnlockKeyBundle unwraps the DEK using the user's master password.
func UnlockKeyBundle(bundle *KeyBundle, password string) ([]byte, error) {
	return UnlockKeyBundleWithKeyfile(bundle, password, nil)
}

// UnlockKeyBundleWithKeyfile unwraps the DEK using password + optional keyfile (supports both legacy AES-GCM and YTPQ cascade).
func UnlockKeyBundleWithKeyfile(bundle *KeyBundle, password string, keyfileBytes []byte) ([]byte, error) {
	if bundle == nil || len(bundle.KDFSalt) == 0 || len(bundle.WrappedDEK) == 0 {
		return nil, errors.New("pacote de chaves inválido")
	}
	if bundle.UsesKeyfile && len(keyfileBytes) == 0 {
		hint := ""
		if bundle.KeyfileHint != "" {
			hint = fmt.Sprintf(" (%s)", bundle.KeyfileHint)
		}
		return nil, fmt.Errorf("este cofre exige o Arquivo-Chave local%s para desbloquear", hint)
	}
	kek := DeriveKEKWithKeyfile(password, keyfileBytes, bundle.KDFSalt, bundle.ArgonTime, bundle.ArgonMemory, bundle.ArgonThreads)
	defer ZeroBytes(kek)

	dek, err := DecryptAuto(kek, bundle.WrappedDEK, []byte("YTCloud-DEK-v1"))
	if err != nil {
		return nil, ErrDecryptFailed
	}
	return dek, nil
}

// RewrapKeyBundle changes the master password while keeping the exact same DEK.
func RewrapKeyBundle(dek []byte, newPassword string) (*KeyBundle, error) {
	return RewrapKeyBundleWithKeyfile(dek, newPassword, nil, "")
}

// RewrapKeyBundleWithKeyfile changes password and/or keyfile while keeping the exact same DEK.
func RewrapKeyBundleWithKeyfile(dek []byte, newPassword string, keyfileBytes []byte, keyfileHint string) (*KeyBundle, error) {
	if len(dek) != KeySize {
		return nil, errors.New("DEK inválida")
	}
	if len(newPassword) < 4 {
		return nil, errors.New("a nova senha deve ter pelo menos 4 caracteres")
	}
	salt, err := RandomBytes(SaltSize)
	if err != nil {
		return nil, err
	}
	kek := DeriveKEKWithKeyfile(newPassword, keyfileBytes, salt, ArgonTime, ArgonMemory, ArgonThreads)
	defer ZeroBytes(kek)

	wrapped, err := EncryptPQ(kek, dek, []byte("YTCloud-DEK-v1"))
	if err != nil {
		return nil, err
	}
	check := sha256.Sum256(append([]byte("YTCloud-Check-v1:"), dek...))
	return &KeyBundle{
		KDFSalt:      salt,
		ArgonTime:    ArgonTime,
		ArgonMemory:  ArgonMemory,
		ArgonThreads: ArgonThreads,
		WrappedDEK:   wrapped,
		KeyCheckHash: hex.EncodeToString(check[:8]),
		UsesKeyfile:  len(keyfileBytes) > 0,
		KeyfileHint:  keyfileHint,
	}, nil
}

// DeriveSubkey derives an independent 32-byte key for a specific container/blob using HKDF-SHA256.
func DeriveSubkey(masterKey, salt []byte, info string) ([]byte, error) {
	r := hkdf.New(sha256.New, masterKey, salt, []byte(info))
	sub := make([]byte, KeySize)
	if _, err := io.ReadFull(r, sub); err != nil {
		return nil, err
	}
	return sub, nil
}

// deriveCascadePair derives two independent 256-bit keys (AES-256 + XChaCha20) via HKDF-SHA512.
func deriveCascadePair(masterKey, salt []byte) (keyAES, keyChaCha []byte, err error) {
	r := hkdf.New(sha512.New, masterKey, salt, []byte("YTCloud-PQ-Cascade-v2"))
	keyAES = make([]byte, KeySize)
	keyChaCha = make([]byte, KeySize)
	if _, err = io.ReadFull(r, keyAES); err != nil {
		return nil, nil, err
	}
	if _, err = io.ReadFull(r, keyChaCha); err != nil {
		ZeroBytes(keyAES)
		return nil, nil, err
	}
	return keyAES, keyChaCha, nil
}

// EncryptPQ applies Post-Quantum Dual-Layer Cascaded AEAD encryption:
// Layer 1: AES-256-GCM (SPN family) -> Layer 2: XChaCha20-Poly1305 (ARX family, 192-bit nonce)
// using two independent 256-bit keys derived via HKDF-SHA512.
// Format: "YTPQ"(4) + salt(16) + xchachaNonce(24) + ciphertext + tag(16)
func EncryptPQ(masterKey, plaintext, aad []byte) ([]byte, error) {
	salt, err := RandomBytes(PQSaltSize)
	if err != nil {
		return nil, err
	}
	keyAES, keyChaCha, err := deriveCascadePair(masterKey, salt)
	if err != nil {
		return nil, err
	}
	defer ZeroBytes(keyAES)
	defer ZeroBytes(keyChaCha)

	aad1 := append([]byte("L1-AES:"), aad...)
	layer1, err := EncryptAESGCM(keyAES, plaintext, aad1)
	if err != nil {
		return nil, err
	}

	aead, err := chacha20poly1305.NewX(keyChaCha)
	if err != nil {
		return nil, err
	}
	nonceX, err := RandomBytes(chacha20poly1305.NonceSizeX)
	if err != nil {
		return nil, err
	}
	aad2 := append([]byte("L2-XCHACHA:"), aad...)
	out := make([]byte, 0, len(PQMagic)+PQSaltSize+len(nonceX)+len(layer1)+aead.Overhead())
	out = append(out, []byte(PQMagic)...)
	out = append(out, salt...)
	out = append(out, nonceX...)
	out = aead.Seal(out, nonceX, layer1, aad2)
	return out, nil
}

// DecryptAuto transparently decrypts both new YTPQ Dual-Layer Cascade blobs and legacy AES-256-GCM blobs.
func DecryptAuto(masterKey, data, aad []byte) ([]byte, error) {
	if len(data) >= len(PQMagic)+PQSaltSize+chacha20poly1305.NonceSizeX+16 && string(data[:4]) == PQMagic {
		salt := data[4 : 4+PQSaltSize]
		rest := data[4+PQSaltSize:]
		keyAES, keyChaCha, err := deriveCascadePair(masterKey, salt)
		if err != nil {
			return nil, ErrDecryptFailed
		}
		defer ZeroBytes(keyAES)
		defer ZeroBytes(keyChaCha)

		aead, err := chacha20poly1305.NewX(keyChaCha)
		if err != nil {
			return nil, ErrDecryptFailed
		}
		ns := chacha20poly1305.NonceSizeX
		aad2 := append([]byte("L2-XCHACHA:"), aad...)
		layer1, err := aead.Open(nil, rest[:ns], rest[ns:], aad2)
		if err != nil {
			return nil, ErrDecryptFailed
		}
		aad1 := append([]byte("L1-AES:"), aad...)
		return DecryptAESGCM(keyAES, layer1, aad1)
	}
	return DecryptAESGCM(masterKey, data, aad)
}

// EncryptAESGCM encrypts plaintext with AES-256-GCM, prepending a random 12-byte nonce.
func EncryptAESGCM(key, plaintext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce, err := RandomBytes(gcm.NonceSize())
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(nonce), len(nonce)+len(plaintext)+gcm.Overhead())
	copy(out, nonce)
	out = gcm.Seal(out, nonce, plaintext, aad)
	return out, nil
}

// DecryptAESGCM decrypts nonce(12)+ciphertext+tag using AES-256-GCM.
func DecryptAESGCM(key, data, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(data) < ns+gcm.Overhead() {
		return nil, ErrDecryptFailed
	}
	pt, err := gcm.Open(nil, data[:ns], data[ns:], aad)
	if err != nil {
		return nil, ErrDecryptFailed
	}
	return pt, nil
}

// EncryptWithPassword encrypts a standalone blob directly with a password using Argon2id + YTPQ Dual-Layer Cascade.
// Format: "YCE2"(4) + salt(32) + YTPQ(...)
func EncryptWithPassword(password string, plaintext, aad []byte) ([]byte, error) {
	return EncryptWithPasswordAndKeyfile(password, nil, plaintext, aad)
}

// EncryptWithPasswordAndKeyfile encrypts a standalone blob using password + optional keyfile + YTPQ cascade.
func EncryptWithPasswordAndKeyfile(password string, keyfileBytes []byte, plaintext, aad []byte) ([]byte, error) {
	salt, err := RandomBytes(SaltSize)
	if err != nil {
		return nil, err
	}
	key := DeriveKEKWithKeyfile(password, keyfileBytes, salt, ArgonTime, ArgonMemory, ArgonThreads)
	defer ZeroBytes(key)

	enc, err := EncryptPQ(key, plaintext, aad)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 4+len(salt)+len(enc))
	out = append(out, []byte("YCE2")...)
	out = append(out, salt...)
	out = append(out, enc...)
	return out, nil
}

// DecryptWithPassword decrypts a blob created by EncryptWithPassword (both legacy YCE1 and new YCE2).
func DecryptWithPassword(password string, blob, aad []byte) ([]byte, error) {
	return DecryptWithPasswordAndKeyfile(password, nil, blob, aad)
}

// DecryptWithPasswordAndKeyfile decrypts YCE1 or YCE2 blobs using password + optional keyfile.
func DecryptWithPasswordAndKeyfile(password string, keyfileBytes []byte, blob, aad []byte) ([]byte, error) {
	if len(blob) < 4+SaltSize+NonceSize+16 {
		return nil, ErrDecryptFailed
	}
	magic := string(blob[:4])
	if magic != "YCE1" && magic != "YCE2" {
		return nil, ErrDecryptFailed
	}
	salt := blob[4 : 4+SaltSize]
	enc := blob[4+SaltSize:]
	key := DeriveKEKWithKeyfile(password, keyfileBytes, salt, ArgonTime, ArgonMemory, ArgonThreads)
	defer ZeroBytes(key)
	return DecryptAuto(key, enc, aad)
}

// SHA256Hex returns the lowercase hex SHA-256 digest of data.
func SHA256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// ZeroBytes overwrites sensitive key buffers in memory.
func ZeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
