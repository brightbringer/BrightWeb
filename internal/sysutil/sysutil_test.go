package sysutil

import (
	"bytes"
	"runtime"
	"testing"
)

func TestDPAPIRoundTrip(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("DPAPI test only on Windows")
	}
	secret := []byte("MinhaSenhaMestraSuperForte!2026")
	enc, err := DPAPIProtect(secret)
	if err != nil {
		t.Fatalf("DPAPIProtect failed: %v", err)
	}
	if bytes.Equal(enc, secret) {
		t.Fatalf("DPAPI output must not match plaintext")
	}
	dec, err := DPAPIUnprotect(enc)
	if err != nil {
		t.Fatalf("DPAPIUnprotect failed: %v", err)
	}
	if !bytes.Equal(dec, secret) {
		t.Fatalf("DPAPIUnprotect mismatch")
	}
}
