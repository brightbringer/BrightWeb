package crypto

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestPQCascadeAndLegacyCompatibility(t *testing.T) {
	key, _ := RandomBytes(32)
	msg := []byte("Dados secretos de teste para cascata pós-quântica AES-256-GCM + XChaCha20-Poly1305!")
	aad := []byte("YTCloud-Test-AAD")

	// 1. New YTPQ dual-layer cascade round-trip
	pqBlob, err := EncryptPQ(key, msg, aad)
	if err != nil {
		t.Fatalf("EncryptPQ failed: %v", err)
	}
	if string(pqBlob[:4]) != PQMagic {
		t.Fatalf("Expected YTPQ magic")
	}
	pt, err := DecryptAuto(key, pqBlob, aad)
	if err != nil || !bytes.Equal(pt, msg) {
		t.Fatalf("DecryptAuto(YTPQ) failed: %v", err)
	}

	// 1-bit tamper must fail
	tampered := append([]byte(nil), pqBlob...)
	tampered[len(tampered)-5] ^= 0x01
	if _, err := DecryptAuto(key, tampered, aad); err == nil {
		t.Fatalf("Expected tamper detection failure")
	}

	// 2. Legacy AES-256-GCM blob must still decrypt seamlessly via DecryptAuto!
	legacyBlob, err := EncryptAESGCM(key, msg, aad)
	if err != nil {
		t.Fatalf("EncryptAESGCM failed: %v", err)
	}
	ptLegacy, err := DecryptAuto(key, legacyBlob, aad)
	if err != nil || !bytes.Equal(ptLegacy, msg) {
		t.Fatalf("DecryptAuto(legacy AES-GCM) failed: %v", err)
	}
}

func TestKeyfileTwoFactorBundleAndRotation(t *testing.T) {
	dir := t.TempDir()
	kfPath := filepath.Join(dir, "meu-chaveiro.ytkey")
	kfBytes, err := GenerateKeyfile(kfPath)
	if err != nil {
		t.Fatalf("GenerateKeyfile failed: %v", err)
	}

	bundle, dek, err := CreateKeyBundleWithKeyfile("SenhaForte#2026", kfBytes, "meu-chaveiro.ytkey")
	if err != nil {
		t.Fatalf("CreateKeyBundleWithKeyfile failed: %v", err)
	}
	if !bundle.UsesKeyfile || bundle.KeyfileHint != "meu-chaveiro.ytkey" {
		t.Fatalf("Unexpected bundle keyfile metadata: %+v", bundle)
	}

	// Without keyfile -> MUST fail!
	if _, err := UnlockKeyBundleWithKeyfile(bundle, "SenhaForte#2026", nil); err == nil {
		t.Fatalf("Expected unlock without keyfile to fail")
	}
	// Wrong keyfile -> MUST fail!
	if _, err := UnlockKeyBundleWithKeyfile(bundle, "SenhaForte#2026", []byte("wrong-file")); err == nil {
		t.Fatalf("Expected unlock with wrong keyfile to fail")
	}
	// Correct password + keyfile -> MUST recover exact DEK!
	gotDEK, err := UnlockKeyBundleWithKeyfile(bundle, "SenhaForte#2026", kfBytes)
	if err != nil || !bytes.Equal(gotDEK, dek) {
		t.Fatalf("UnlockKeyBundleWithKeyfile failed: %v", err)
	}

	// Rotate to remove keyfile -> DEK stays identical!
	bundleNoKF, err := RewrapKeyBundleWithKeyfile(dek, "NovaSenha#99", nil, "")
	if err != nil {
		t.Fatalf("Rewrap failed: %v", err)
	}
	gotDEK2, err := UnlockKeyBundleWithKeyfile(bundleNoKF, "NovaSenha#99", nil)
	if err != nil || !bytes.Equal(gotDEK2, dek) {
		t.Fatalf("DEK mismatch after keyfile removal")
	}
}
