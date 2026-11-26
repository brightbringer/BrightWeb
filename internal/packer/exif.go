package packer

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ApplyExportMetadata updates filesystem timestamps and (for JPEGs) writes a clean EXIF
// DateTimeOriginal / DateTimeDigitized / ModifyDate APP1 segment so restored old photos
// preserve chronological order in Windows Explorer, Google Photos, Apple Photos, etc.
func ApplyExportMetadata(filePath string, effectiveDate string) error {
	t, ok := ParseFlexibleDate(effectiveDate)
	if !ok {
		return nil
	}
	ext := strings.ToLower(filepath.Ext(filePath))
	if ext == ".jpg" || ext == ".jpeg" {
		_ = WriteJPEGExifDate(filePath, t)
	}
	return os.Chtimes(filePath, t, t)
}

// ParseFlexibleDate parses exact ISO timestamps or approximate dates ("1998", "1998-07", "1998-07-14").
func ParseFlexibleDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
		"2006-01",
		"2006",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// WriteJPEGExifDate injects or replaces the EXIF APP1 block with DateTimeOriginal & ModifyDate.
// Zero external dependencies (pure Go), keeping YTCloud.exe tiny.
func WriteJPEGExifDate(path string, t time.Time) error {
	data, err := os.ReadFile(path)
	if err != nil || len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return err
	}

	dateStr := t.Format("2006:01:02 15:04:05") + "\x00" // 20 bytes including NUL
	exifSeg := buildMinimalExifAPP1(dateStr)

	// Walk existing JPEG markers right after SOI (FF D8) and strip any old Exif APP1
	var out bytes.Buffer
	out.Write([]byte{0xFF, 0xD8})
	out.Write(exifSeg)

	pos := 2
	for pos+4 <= len(data) {
		if data[pos] != 0xFF {
			break
		}
		marker := data[pos+1]
		if marker == 0xDA || marker == 0xD9 { // SOS or EOI: write rest verbatim
			break
		}
		segLen := int(binary.BigEndian.Uint16(data[pos+2 : pos+4]))
		if segLen < 2 || pos+2+segLen > len(data) {
			break
		}
		isOldExif := marker == 0xE1 && segLen >= 8 && bytes.Equal(data[pos+4:pos+10], []byte("Exif\x00\x00"))
		if !isOldExif {
			out.Write(data[pos : pos+2+segLen])
		}
		pos += 2 + segLen
	}
	out.Write(data[pos:])
	return os.WriteFile(path, out.Bytes(), 0o644)
}

func buildMinimalExifAPP1(date20 string) []byte {
	// Little-endian TIFF header + IFD0 (ModifyDate 0x0132, ExifIFD ptr 0x8769) + ExifIFD (DateTimeOriginal 0x9003, DateTimeDigitized 0x9004)
	var tiff bytes.Buffer
	// TIFF header (8 bytes): "II", 42, offset to IFD0 = 8
	tiff.Write([]byte{'I', 'I'})
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(42))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(8))

	// Offsets:
	// IFD0 starts at 8: 2 (count=2) + 2*12 (entries) + 4 (next=0) = 30 bytes -> ends at 38
	// ExifIFD starts at 38: 2 (count=2) + 2*12 (entries) + 4 (next=0) = 30 bytes -> ends at 68
	// Date string (20 bytes) stored at offset 68 (shared cleanly by ModifyDate, DateTimeOriginal, DateTimeDigitized)
	const (
		exifIFDOff = 38
		dateOff    = 68
	)

	writeIFDEntry := func(tag, typ uint16, count, valOrOff uint32) {
		_ = binary.Write(&tiff, binary.LittleEndian, tag)
		_ = binary.Write(&tiff, binary.LittleEndian, typ)
		_ = binary.Write(&tiff, binary.LittleEndian, count)
		_ = binary.Write(&tiff, binary.LittleEndian, valOrOff)
	}

	// IFD0
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(2))
	writeIFDEntry(0x0132, 2, 20, dateOff)       // ModifyDate (ASCII, 20 bytes)
	writeIFDEntry(0x8769, 4, 1, exifIFDOff)     // ExifIFD pointer (LONG)
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(0))

	// ExifIFD
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(2))
	writeIFDEntry(0x9003, 2, 20, dateOff) // DateTimeOriginal
	writeIFDEntry(0x9004, 2, 20, dateOff) // DateTimeDigitized
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(0))

	// ASCII date string
	tiff.WriteString(date20)

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	var seg bytes.Buffer
	seg.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(&seg, binary.BigEndian, uint16(len(payload)+2))
	seg.Write(payload)
	return seg.Bytes()
}
