package crypto

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

const (
	// PublicShareMagic identifies standalone public-share containers decryptable strictly
	// from the video + 1..5 layered passwords (zero vault access required!).
	PublicShareMagic = "YTPUB001"
	// OpenWebMagic identifies passwordless open optical web containers readable directly by YTBrowser.
	OpenWebMagic = "YTWB001\x00"
	// ShardMagic identifies a Multi-Video Parallel Shard container (YTWeb Optical BitTorrent / Sharding).
	ShardMagic      = "YTSH001\x00"
	MaxPublicLayers = 5
)

// NormalizePublicPasswords trims empty entries and validates 1..MaxPublicLayers passwords.
func NormalizePublicPasswords(passwords []string) ([]string, error) {
	out := make([]string, 0, len(passwords))
	for _, p := range passwords {
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("informe pelo menos 1 senha para o modo público")
	}
	if len(out) > MaxPublicLayers {
		return nil, fmt.Errorf("máximo de %d senhas em cascata suportadas", MaxPublicLayers)
	}
	return out, nil
}

// derivePublicLayerKey derives an independent 256-bit Argon2id key for layer idx using a domain-separated salt.
func derivePublicLayerKey(password string, salt []byte, layerIdx int) []byte {
	h := sha256.New()
	h.Write([]byte("YTCloud-Public-Layer:"))
	h.Write([]byte{byte(layerIdx)})
	h.Write(salt)
	layerSalt := h.Sum(nil)
	return DeriveKEK(password, layerSalt, ArgonTime, ArgonMemory, ArgonThreads)
}

// EncryptPublicCascade encrypts plaintext through N independent Post-Quantum layers
// (each password derives its own Argon2id key and wraps the data in AES-256-GCM + XChaCha20-Poly1305!).
func EncryptPublicCascade(passwords []string, salt, plaintext, aad []byte) ([]byte, error) {
	clean, err := NormalizePublicPasswords(passwords)
	if err != nil {
		return nil, err
	}
	if len(salt) < 16 {
		return nil, errors.New("salt público inválido")
	}
	cur := plaintext
	for i, pw := range clean {
		key := derivePublicLayerKey(pw, salt, i)
		layerAAD := append(append([]byte{}, aad...), byte(':'), byte('0'+i))
		next, err := EncryptPQ(key, cur, layerAAD)
		ZeroBytes(key)
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// DecryptPublicCascade peels N independent Post-Quantum layers in reverse order.
func DecryptPublicCascade(passwords []string, salt, ciphertext, aad []byte) ([]byte, error) {
	clean, err := NormalizePublicPasswords(passwords)
	if err != nil {
		return nil, err
	}
	if len(salt) < 16 {
		return nil, errors.New("salt público inválido")
	}
	cur := ciphertext
	for i := len(clean) - 1; i >= 0; i-- {
		key := derivePublicLayerKey(clean[i], salt, i)
		layerAAD := append(append([]byte{}, aad...), byte(':'), byte('0'+i))
		next, err := DecryptAuto(key, cur, layerAAD)
		ZeroBytes(key)
		if err != nil {
			if len(clean) > 1 {
				return nil, fmt.Errorf("falha na camada de senha %d/%d: senha incorreta ou ordem trocada", i+1, len(clean))
			}
			return nil, ErrDecryptFailed
		}
		cur = next
	}
	return cur, nil
}

// WrapPublicManifest builds the self-describing public manifest envelope:
// [8B "YTPUB001"][1B layerCount][16B salt][16B contUUID][encManifest]
func WrapPublicManifest(layerCount int, salt []byte, contUUID string, encManifest []byte) []byte {
	out := make([]byte, 0, 8+1+16+16+len(encManifest))
	out = append(out, []byte(PublicShareMagic)...)
	out = append(out, byte(layerCount))
	out = append(out, salt[:16]...)
	uuidBytes := []byte(strings.TrimSpace(contUUID))
	if len(uuidBytes) > 16 {
		uuidBytes = uuidBytes[:16]
	}
	for len(uuidBytes) < 16 {
		uuidBytes = append(uuidBytes, '0')
	}
	out = append(out, uuidBytes...)
	out = append(out, encManifest...)
	return out
}

// ParsePublicManifest detects whether a wrapped manifest belongs to a Public Share container.
// Returns (isPublic, layerCount, salt, contUUID, encManifest, err).
func ParsePublicManifest(wrapped []byte) (bool, int, []byte, string, []byte, error) {
	if len(wrapped) < 8 || !bytes.Equal(wrapped[:8], []byte(PublicShareMagic)) {
		return false, 0, nil, "", nil, nil
	}
	if len(wrapped) < 8+1+16+16+4 {
		return true, 0, nil, "", nil, errors.New("cabeçalho público truncado")
	}
	layers := int(wrapped[8])
	if layers < 1 || layers > MaxPublicLayers {
		return true, 0, nil, "", nil, fmt.Errorf("número de camadas de senha inválido (%d)", layers)
	}
	salt := make([]byte, 16)
	copy(salt, wrapped[9:25])
	contUUID := string(wrapped[25:41])
	encManifest := wrapped[41:]
	return true, layers, salt, contUUID, encManifest, nil
}

// WrapOpenWebManifest wraps an unencrypted public optical-web manifest with the YTWB001 envelope:
// [8B "YTWB001\x00"][16B contUUID][manifestJSON]
func WrapOpenWebManifest(contUUID string, manifestJSON []byte) []byte {
	out := make([]byte, 0, 8+16+len(manifestJSON))
	out = append(out, []byte(OpenWebMagic)...)
	uuidBytes := []byte(strings.TrimSpace(contUUID))
	if len(uuidBytes) > 16 {
		uuidBytes = uuidBytes[:16]
	}
	for len(uuidBytes) < 16 {
		uuidBytes = append(uuidBytes, '0')
	}
	out = append(out, uuidBytes...)
	out = append(out, manifestJSON...)
	return out
}

// ParseOpenWebManifest detects whether a wrapped manifest belongs to a passwordless YTWB001 optical site.
func ParseOpenWebManifest(wrapped []byte) (bool, string, []byte, error) {
	if len(wrapped) < 8 || !bytes.Equal(wrapped[:8], []byte(OpenWebMagic)) {
		return false, "", nil, nil
	}
	if len(wrapped) < 8+16+2 {
		return true, "", nil, errors.New("cabeçalho YTWeb aberto truncado")
	}
	contUUID := string(wrapped[8:24])
	return true, contUUID, wrapped[24:], nil
}

// ShardInfo describes one video part in a Multi-Video Parallel Sharded optical website/container.
type ShardInfo struct {
	GroupID            string
	ShardIndex         int      // 0-based (0..ShardCount-1)
	ShardCount         int      // total parallel video shards (2..64)
	TotalPayloadLen    uint64   // exact total byte length of reassembled payload across all shards
	TotalPayloadSHA256 [32]byte // SHA-256 of the complete reassembled payload
	InnerManifest      []byte   // full YTWB001 or YTPUB001 manifest (stored on shard 0; empty on shards 1..N-1)
}

// WrapShardManifest builds the 68-byte-header YTSH001 multi-shard manifest envelope:
// [8B "YTSH001\x00"][16B groupID][2B idx LE][2B count LE][8B totalLen LE][32B sha256][innerManifest]
func WrapShardManifest(info ShardInfo) []byte {
	out := make([]byte, 0, 68+len(info.InnerManifest))
	out = append(out, []byte(ShardMagic)...)
	gid := []byte(strings.TrimSpace(info.GroupID))
	if len(gid) > 16 {
		gid = gid[:16]
	}
	for len(gid) < 16 {
		gid = append(gid, '0')
	}
	out = append(out, gid...)
	var meta [12]byte
	binary.LittleEndian.PutUint16(meta[0:2], uint16(info.ShardIndex))
	binary.LittleEndian.PutUint16(meta[2:4], uint16(info.ShardCount))
	binary.LittleEndian.PutUint64(meta[4:12], info.TotalPayloadLen)
	out = append(out, meta[:]...)
	out = append(out, info.TotalPayloadSHA256[:]...)
	out = append(out, info.InnerManifest...)
	return out
}

// ParseShardManifest detects whether wrapped is a YTSH001 multi-shard manifest envelope.
func ParseShardManifest(wrapped []byte) (bool, ShardInfo, error) {
	if len(wrapped) < 8 || !bytes.Equal(wrapped[:8], []byte(ShardMagic)) {
		return false, ShardInfo{}, nil
	}
	if len(wrapped) < 68 {
		return true, ShardInfo{}, errors.New("cabeçalho Multi-Shard YTSH001 truncado")
	}
	idx := int(binary.LittleEndian.Uint16(wrapped[24:26]))
	cnt := int(binary.LittleEndian.Uint16(wrapped[26:28]))
	if cnt < 2 || cnt > 64 || idx < 0 || idx >= cnt {
		return true, ShardInfo{}, fmt.Errorf("metadados de shard inválidos (%d/%d)", idx+1, cnt)
	}
	info := ShardInfo{
		GroupID:         string(wrapped[8:24]),
		ShardIndex:      idx,
		ShardCount:      cnt,
		TotalPayloadLen: binary.LittleEndian.Uint64(wrapped[28:36]),
		InnerManifest:   wrapped[68:],
	}
	copy(info.TotalPayloadSHA256[:], wrapped[36:68])
	return true, info, nil
}
