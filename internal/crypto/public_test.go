package crypto

import (
	"bytes"
	"testing"
)

func TestPublicCascadeMultiPasswordRoundTrip(t *testing.T) {
	salt, err := RandomBytes(16)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("YTCloud public post-quantum multi-password payload 1234567890")
	aad := []byte("YTCloud-Public-Payload:0123456789abcdef")

	// Test 1, 2, and 3 layered passwords
	cases := [][]string{
		{"senha-unica-forte"},
		{"camada-um-alfa", "camada-dois-beta"},
		{"chave-1", "chave-2", "chave-3-pos-ia"},
	}

	for _, pws := range cases {
		ct, err := EncryptPublicCascade(pws, salt, plaintext, aad)
		if err != nil {
			t.Fatalf("EncryptPublicCascade(%d layers): %v", len(pws), err)
		}
		dec, err := DecryptPublicCascade(pws, salt, ct, aad)
		if err != nil {
			t.Fatalf("DecryptPublicCascade(%d layers): %v", len(pws), err)
		}
		if !bytes.Equal(dec, plaintext) {
			t.Fatalf("plaintext mismatch for %d layers", len(pws))
		}

		// Wrong password must fail
		bad := append([]string{}, pws...)
		bad[len(bad)-1] += "X"
		if _, err := DecryptPublicCascade(bad, salt, ct, aad); err == nil {
			t.Fatalf("expected failure with wrong password (%d layers)", len(pws))
		}

		// Swapped password order must fail when >= 2 layers
		if len(pws) >= 2 {
			swapped := append([]string{}, pws...)
			swapped[0], swapped[1] = swapped[1], swapped[0]
			if _, err := DecryptPublicCascade(swapped, salt, ct, aad); err == nil {
				t.Fatalf("expected failure with swapped password order (%d layers)", len(pws))
			}
		}

		// Envelope wrap & parse
		wrapped := WrapPublicManifest(len(pws), salt, "0123456789abcdef", ct)
		isPub, layers, gotSalt, gotUUID, gotCT, err := ParsePublicManifest(wrapped)
		if err != nil || !isPub || layers != len(pws) || !bytes.Equal(gotSalt, salt) || gotUUID != "0123456789abcdef" || !bytes.Equal(gotCT, ct) {
			t.Fatalf("ParsePublicManifest failed: isPub=%v layers=%d err=%v", isPub, layers, err)
		}
	}

	// Non-public private manifest must return isPub=false, err=nil
	priv := make([]byte, 64)
	if isPub, _, _, _, _, err := ParsePublicManifest(priv); isPub || err != nil {
		t.Fatalf("expected private manifest not to match YTPUB001: isPub=%v err=%v", isPub, err)
	}
}
