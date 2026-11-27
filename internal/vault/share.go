package vault

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const (
	FolderSharePrefix       = "BVFOLDER1:"
	LegacyFolderSharePrefix = "YTFOLDER1:"
)

// FolderSharePayload is a point-in-time snapshot of a folder's files, YouTube video links,
// and exact per-container decryption keys — isolated from the Master Vault key and other folders.
type FolderSharePayload struct {
	Version           int                    `json:"v"`
	FolderName        string                 `json:"name"`
	CreatedAt         string                 `json:"created_at"`
	IncludeSubfolders bool                   `json:"subfolders,omitempty"`
	Containers        []FolderShareContainer `json:"containers"`
	Files             []FolderShareFile      `json:"files"`
}

type FolderShareContainer struct {
	ID             string `json:"id"`
	ShortCode      string `json:"code"`
	YouTubeURL     string `json:"url"`
	YouTubeVideoID string `json:"vid,omitempty"`
	Privacy        string `json:"privacy,omitempty"`
	KeyHex         string `json:"key"` // 32-byte derived container key (hex)
}

type FolderShareFile struct {
	Name          string `json:"n"`
	RelPath       string `json:"rp"`
	FolderRelPath string `json:"fp,omitempty"`
	SizeBytes     int64  `json:"sz"`
	EffectiveDate string `json:"dt,omitempty"`
	ContainerID   string `json:"cid"`
}

// EncodeFolderShareCode compresses and encodes a point-in-time FolderSharePayload as "YTFOLDER1:<b64url>".
func EncodeFolderShareCode(p *FolderSharePayload) (string, error) {
	if p == nil {
		return "", errors.New("payload de compartilhamento vazio")
	}
	if p.Version == 0 {
		p.Version = 1
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
	if err != nil {
		return "", err
	}
	comp := enc.EncodeAll(raw, nil)
	_ = enc.Close()
	return FolderSharePrefix + base64.RawURLEncoding.EncodeToString(comp), nil
}

// DecodeFolderShareCode parses a "YTFOLDER1:<b64url>" folder sharing code.
func DecodeFolderShareCode(code string) (*FolderSharePayload, error) {
	clean := strings.TrimSpace(code)
	// Strip accidental whitespace/newlines from copy-paste
	clean = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, clean)
	var b64 string
	switch {
	case strings.HasPrefix(clean, FolderSharePrefix):
		b64 = strings.TrimPrefix(clean, FolderSharePrefix)
	case strings.HasPrefix(clean, LegacyFolderSharePrefix):
		b64 = strings.TrimPrefix(clean, LegacyFolderSharePrefix)
	default:
		return nil, errors.New("código de pasta inválido (deve começar com BVFOLDER1: ou YTFOLDER1:)")
	}
	comp, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("código de pasta corrompido (base64): %w", err)
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	raw, err := dec.DecodeAll(comp, nil)
	if err != nil {
		return nil, fmt.Errorf("código de pasta corrompido (zstd): %w", err)
	}
	var p FolderSharePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("código de pasta inválido (JSON): %w", err)
	}
	if p.Version != 1 {
		return nil, fmt.Errorf("versão do código de pasta não suportada: %d", p.Version)
	}
	return &p, nil
}
