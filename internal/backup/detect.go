package backup

import (
	"bytes"
	"path"
	"regexp"
	"strings"
	"time"
)

var (
	ageMagic      = []byte("age-encryption.org/v1\n")
	zstdMagic     = []byte{0x28, 0xb5, 0x2f, 0xfd}
	gzipMagic     = []byte{0x1f, 0x8b}
	backupStampRe = regexp.MustCompile(`(\d{8}-\d{6})`)
)

// HasAgeHeader reports whether head starts with the standard age v1 header.
func HasAgeHeader(head []byte) bool {
	return bytes.HasPrefix(head, ageMagic)
}

// ParsedBackup holds metadata inferred from a backup object's key or filename.
type ParsedBackup struct {
	Recognized  bool      `json:"recognized"`
	Kind        string    `json:"kind"`        // database | volume | dir | panel | ""
	Format      string    `json:"format"`      // pgdump | sql | archive | rdb | tar | sqlite | ""
	EngineHint  string    `json:"engineHint"`  // postgres | mysql | mongo | redis | ""
	Compression string    `json:"compression"` // zst | gz | none
	Encrypted   bool      `json:"encrypted"`
	Label       string    `json:"label"`
	VolumeHint  string    `json:"volumeHint,omitempty"`
	Timestamp   time.Time `json:"-"`
}

// IsIgnoredObjectKey reports whether a storage object key should be skipped during scanning.
func IsIgnoredObjectKey(key string) bool {
	clean := strings.Trim(strings.ReplaceAll(key, "\\", "/"), "/")
	if clean == "" || strings.HasSuffix(clean, "/") {
		return true
	}
	lower := strings.ToLower(clean)
	if strings.HasSuffix(lower, ".part") {
		return true
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// ParseBackupKey infers backup metadata from key, parsing timestamps in UTC.
func ParseBackupKey(key string) ParsedBackup {
	return ParseBackupKeyInLocation(key, time.UTC)
}

// ParseBackupKeyInLocation infers backup kind, format, compression, encryption, label and
// timestamp (interpreted in loc, defaulting to UTC if nil) from key.
func ParseBackupKeyInLocation(key string, loc *time.Location) ParsedBackup {
	if loc == nil {
		loc = time.UTC
	}
	clean := strings.Trim(strings.ReplaceAll(key, "\\", "/"), "/")
	dir, base := path.Split(clean)
	dir = strings.Trim(dir, "/")

	out := ParsedBackup{Compression: "none"}
	name := base
	lower := strings.ToLower(name)

	if strings.HasSuffix(lower, ".age") {
		out.Encrypted = true
		name = name[:len(name)-4]
		lower = strings.ToLower(name)
	}

	switch {
	case strings.HasSuffix(lower, ".zst"):
		out.Compression = "zst"
		name = name[:len(name)-4]
	case strings.HasSuffix(lower, ".tgz"):
		out.Compression = "gz"
		name = name[:len(name)-4] + ".tar"
	case strings.HasSuffix(lower, ".gz"):
		out.Compression = "gz"
		name = name[:len(name)-3]
	}

	lower = strings.ToLower(name)
	ext := path.Ext(lower)
	stem := strings.TrimSuffix(name, path.Ext(name))

	switch ext {
	case ".pgdump", ".dump", ".backup":
		out.Recognized = true
		out.Kind = "database"
		out.Format = "pgdump"
		out.EngineHint = "postgres"
	case ".sql":
		out.Recognized = true
		out.Kind = "database"
		out.Format = "sql"
	case ".archive":
		out.Recognized = true
		out.Kind = "database"
		out.Format = "archive"
		out.EngineHint = "mongo"
	case ".rdb":
		out.Recognized = true
		out.Kind = "database"
		out.Format = "rdb"
		out.EngineHint = "redis"
	case ".sqlite", ".db", ".sqlite3":
		out.Recognized = true
		out.Kind = "panel"
		out.Format = "sqlite"
	case ".tar":
		out.Recognized = true
		out.Format = "tar"
		firstSeg := dir
		if i := strings.IndexByte(firstSeg, '/'); i >= 0 {
			firstSeg = firstSeg[:i]
		}
		switch {
		case strings.HasPrefix(firstSeg, "volume_"):
			out.Kind = "volume"
			out.VolumeHint = strings.TrimPrefix(firstSeg, "volume_")
		case strings.HasPrefix(stem, "volume_"):
			out.Kind = "volume"
			out.VolumeHint = strings.TrimPrefix(stem, "volume_")
		default:
			out.Kind = "dir"
		}
	}

	if m := backupStampRe.FindStringSubmatch(stem); len(m) == 2 {
		if ts, err := time.ParseInLocation("20060102-150405", m[1], loc); err == nil {
			out.Timestamp = ts
		}
	}

	if out.Kind == "panel" {
		out.Label = "panel"
	} else if dir != "" && dir != "imported" && dir != "_panel" {
		out.Label = dir
	} else if stem != "" {
		out.Label = stem
	} else {
		out.Label = base
	}
	return out
}
