package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
)

func TestOpenStreamAutoDetectAndKeyring(t *testing.T) {
	id1, rcp1Str, err := NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity 1: %v", err)
	}
	id2, rcp2Str, err := NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity 2: %v", err)
	}
	idWrong, _, err := NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity 3: %v", err)
	}
	rcp1, _ := age.ParseX25519Recipient(rcp1Str)
	rcp2, _ := age.ParseX25519Recipient(rcp2Str)

	plainPayload := []byte("hello compose-panel backup stream payload 0123456789")

	compressZstd := func(in []byte) []byte {
		var b bytes.Buffer
		w, err := zstd.NewWriter(&b)
		if err != nil {
			t.Fatalf("zstd writer: %v", err)
		}
		_, _ = w.Write(in)
		_ = w.Close()
		return b.Bytes()
	}
	compressGzip := func(in []byte) []byte {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		_, _ = w.Write(in)
		_ = w.Close()
		return b.Bytes()
	}
	encryptAge := func(in []byte, rcp age.Recipient) []byte {
		var b bytes.Buffer
		w, err := age.Encrypt(&b, rcp)
		if err != nil {
			t.Fatalf("age encrypt: %v", err)
		}
		_, _ = w.Write(in)
		_ = w.Close()
		return b.Bytes()
	}

	cases := []struct {
		name       string
		data       []byte
		encrypted  bool
		identities []string
		wantErr    bool
	}{
		{name: "plain unencrypted", data: plainPayload},
		{name: "gzip unencrypted", data: compressGzip(plainPayload)},
		{name: "zstd unencrypted", data: compressZstd(plainPayload)},
		{name: "zstd age single key", data: encryptAge(compressZstd(plainPayload), rcp1), encrypted: true, identities: []string{id1}},
		{name: "gzip age magic-only without flag", data: encryptAge(compressGzip(plainPayload), rcp2), encrypted: false, identities: []string{id1, id2}},
		{name: "plain age multi-key keyring", data: encryptAge(plainPayload, rcp2), encrypted: true, identities: []string{idWrong, id1, id2}},
		{name: "age wrong key error", data: encryptAge(compressZstd(plainPayload), rcp1), encrypted: true, identities: []string{idWrong, id2}, wantErr: true},
		{name: "age missing key error", data: encryptAge(plainPayload, rcp1), encrypted: true, identities: nil, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(tc.name, "age") && !HasAgeHeader(tc.data) {
				t.Fatalf("expected HasAgeHeader to be true")
			}
			rc, err := OpenStream(io.NopCloser(bytes.NewReader(tc.data)), tc.encrypted, tc.identities)
			if tc.wantErr {
				if err == nil {
					_, rerr := io.ReadAll(rc)
					rc.Close()
					if rerr == nil {
						t.Fatalf("expected error, got nil")
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("OpenStream error: %v", err)
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("ReadAll error: %v", err)
			}
			if !bytes.Equal(got, plainPayload) {
				t.Fatalf("got %q, want %q", got, plainPayload)
			}
		})
	}

	// Verify OpenObject variadic signature works with Local storage.
	st := &Local{Root: t.TempDir()}
	encData := encryptAge(compressZstd(plainPayload), rcp2)
	if _, err := st.Put(context.Background(), "demo/20261120-030000.sql.zst.age", bytes.NewReader(encData)); err != nil {
		t.Fatalf("Local.Put: %v", err)
	}
	rc, err := OpenObject(context.Background(), st, "demo/20261120-030000.sql.zst.age", true, id1, id2)
	if err != nil {
		t.Fatalf("OpenObject variadic error: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("OpenObject ReadAll: %v", err)
	}
	if !bytes.Equal(got, plainPayload) {
		t.Fatalf("OpenObject got %q, want %q", got, plainPayload)
	}
}

func TestParseBackupKeyAndIgnoreRules(t *testing.T) {
	wantTS := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)

	cases := []struct {
		key         string
		recognized  bool
		kind        string
		format      string
		engineHint  string
		compression string
		encrypted   bool
		volumeHint  string
		hasTime     bool
	}{
		{
			key: "pg-main_mydb/20261120-030000.pgdump.zst.age",
			recognized: true, kind: "database", format: "pgdump", engineHint: "postgres",
			compression: "zst", encrypted: true, hasTime: true,
		},
		{
			key: "mysql-1_app/20261120-030000.sql.gz",
			recognized: true, kind: "database", format: "sql",
			compression: "gz", encrypted: false, hasTime: true,
		},
		{
			key: "mongo-1_shop/20261120-030000.archive.zst",
			recognized: true, kind: "database", format: "archive", engineHint: "mongo",
			compression: "zst", encrypted: false, hasTime: true,
		},
		{
			key: "redis-1_all/20261120-030000.rdb.zst.age",
			recognized: true, kind: "database", format: "rdb", engineHint: "redis",
			compression: "zst", encrypted: true, hasTime: true,
		},
		{
			key: "volume_app_data/20261120-030000.tar.zst",
			recognized: true, kind: "volume", format: "tar",
			compression: "zst", encrypted: false, volumeHint: "app_data", hasTime: true,
		},
		{
			key: "myapp_uploads/20261120-030000.tar.zst",
			recognized: true, kind: "dir", format: "tar",
			compression: "zst", encrypted: false, hasTime: true,
		},
		{
			key: "assets.tgz",
			recognized: true, kind: "dir", format: "tar",
			compression: "gz", encrypted: false, hasTime: false,
		},
		{
			key: "_panel/panel-20261120-030000.db",
			recognized: true, kind: "panel", format: "sqlite",
			compression: "none", encrypted: false, hasTime: true,
		},
		{
			key: "panel/20261120-030000.sqlite.zst.age",
			recognized: true, kind: "panel", format: "sqlite",
			compression: "zst", encrypted: true, hasTime: true,
		},
		{
			key: "notes/readme.txt",
			recognized: false, compression: "none", encrypted: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			got := ParseBackupKey(tc.key)
			if got.Recognized != tc.recognized {
				t.Errorf("Recognized = %v, want %v", got.Recognized, tc.recognized)
			}
			if got.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", got.Kind, tc.kind)
			}
			if got.Format != tc.format {
				t.Errorf("Format = %q, want %q", got.Format, tc.format)
			}
			if got.EngineHint != tc.engineHint {
				t.Errorf("EngineHint = %q, want %q", got.EngineHint, tc.engineHint)
			}
			if got.Compression != tc.compression {
				t.Errorf("Compression = %q, want %q", got.Compression, tc.compression)
			}
			if got.Encrypted != tc.encrypted {
				t.Errorf("Encrypted = %v, want %v", got.Encrypted, tc.encrypted)
			}
			if tc.volumeHint != "" && got.VolumeHint != tc.volumeHint {
				t.Errorf("VolumeHint = %q, want %q", got.VolumeHint, tc.volumeHint)
			}
			if tc.hasTime {
				if got.Timestamp.IsZero() {
					t.Errorf("expected non-zero Timestamp")
				} else if got.Timestamp.Format("20060102-150405") != wantTS.Format("20060102-150405") {
					t.Errorf("Timestamp = %v, want %v", got.Timestamp, wantTS)
				}
			} else if !got.Timestamp.IsZero() {
				t.Errorf("expected zero Timestamp, got %v", got.Timestamp)
			}
		})
	}

	ignored := []string{
		".panel-write-test",
		"sub/.panel-write-test",
		"foo/bar.zst.part",
		".DS_Store",
		"dir/.hidden",
	}
	for _, k := range ignored {
		if !IsIgnoredObjectKey(k) {
			t.Errorf("IsIgnoredObjectKey(%q) = false, want true", k)
		}
	}
	kept := []string{
		"_panel/panel-20261120-030000.db",
		"pg-main_mydb/20261120-030000.pgdump.zst.age",
		"imported/20261120-030000-dump.sql",
	}
	for _, k := range kept {
		if IsIgnoredObjectKey(k) {
			t.Errorf("IsIgnoredObjectKey(%q) = true, want false", k)
		}
	}
}

func TestParseBackupKeyInLocation(t *testing.T) {
	shanghai := time.FixedZone("Asia/Shanghai", 8*3600)
	got := ParseBackupKeyInLocation("pg-main_app/20261120-030000.pgdump.zst.age", shanghai)
	want := time.Date(2026, 11, 20, 3, 0, 0, 0, shanghai)
	if !got.Timestamp.Equal(want) {
		t.Fatalf("Timestamp = %v (%d), want %v (%d)", got.Timestamp, got.Timestamp.Unix(), want, want.Unix())
	}
}

func TestUntarDirBlocksSymlinkTraversal(t *testing.T) {
	makeTar := func(entries []struct {
		name     string
		typeflag byte
		linkname string
		body     string
	}) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, e := range entries {
			hdr := &tar.Header{
				Name:     e.name,
				Typeflag: e.typeflag,
				Linkname: e.linkname,
				Mode:     0o644,
				Size:     int64(len(e.body)),
			}
			if e.typeflag == tar.TypeDir {
				hdr.Mode = 0o755
			}
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
			if e.typeflag == tar.TypeReg {
				if _, err := tw.Write([]byte(e.body)); err != nil {
					t.Fatal(err)
				}
			}
		}
		_ = tw.Close()
		return buf.Bytes()
	}

	// 1. Absolute symlink + file through symlink MUST fail and not write outside dir.
	outside := t.TempDir()
	dst1 := filepath.Join(t.TempDir(), "extract1")
	tar1 := makeTar([]struct {
		name     string
		typeflag byte
		linkname string
		body     string
	}{
		{name: "sub", typeflag: tar.TypeSymlink, linkname: outside},
		{name: "sub/pwned.txt", typeflag: tar.TypeReg, body: "owned"},
	})
	if err := UntarDir(bytes.NewReader(tar1), dst1); err == nil {
		t.Fatalf("expected absolute symlink tar-slip to fail")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned.txt")); !os.IsNotExist(err) {
		t.Fatalf("tar-slip wrote outside target dir!")
	}

	// 2. Relative symlink escaping target dir MUST fail.
	dst2 := filepath.Join(t.TempDir(), "extract2")
	tar2 := makeTar([]struct {
		name     string
		typeflag byte
		linkname string
		body     string
	}{
		{name: "nested/escape", typeflag: tar.TypeSymlink, linkname: "../../outside"},
	})
	if err := UntarDir(bytes.NewReader(tar2), dst2); err == nil {
		t.Fatalf("expected escaping relative symlink to fail")
	}

	// 3. Hardlink entry MUST fail.
	dst3 := filepath.Join(t.TempDir(), "extract3")
	tar3 := makeTar([]struct {
		name     string
		typeflag byte
		linkname string
		body     string
	}{
		{name: "hard", typeflag: tar.TypeLink, linkname: "/etc/passwd"},
	})
	if err := UntarDir(bytes.NewReader(tar3), dst3); err == nil {
		t.Fatalf("expected hardlink entry to be rejected")
	}

	// 4. Safe internal relative symlink stays allowed.
	dst4 := filepath.Join(t.TempDir(), "extract4")
	tar4 := makeTar([]struct {
		name     string
		typeflag byte
		linkname string
		body     string
	}{
		{name: "b/ok.txt", typeflag: tar.TypeReg, body: "safe"},
		{name: "a/link.txt", typeflag: tar.TypeSymlink, linkname: "../b/ok.txt"},
	})
	if err := UntarDir(bytes.NewReader(tar4), dst4); err != nil {
		t.Fatalf("safe internal symlink rejected: %v", err)
	}
}

func TestFormatAndEngineCompatibilityMatrix(t *testing.T) {
	normCases := map[string]string{
		"PGDUMP":   "pgdump",
		" .dump ":  "pgdump",
		"backup":   "pgdump",
		".SQL":     "sql",
		"archive":  "archive",
		"rdb":      "rdb",
		"tgz":      "tar",
		".db":      "sqlite",
		"sqlite3":  "sqlite",
		" unknown": "unknown",
	}
	for in, want := range normCases {
		if got := NormalizeBackupFormat(in); got != want {
			t.Errorf("NormalizeBackupFormat(%q) = %q, want %q", in, got, want)
		}
	}

	for _, k := range []string{"database", "volume", "dir", "panel"} {
		if !ValidBackupKind(k) {
			t.Errorf("ValidBackupKind(%q) = false, want true", k)
		}
	}
	if ValidBackupKind("") || ValidBackupKind("other") {
		t.Errorf("ValidBackupKind accepted invalid kind")
	}

	for _, f := range []string{"pgdump", "dump", "sql", "archive", "rdb", "tar", "tgz", "sqlite", "db"} {
		if !ValidBackupFormat(f) {
			t.Errorf("ValidBackupFormat(%q) = false, want true", f)
		}
	}
	if ValidBackupFormat("") || ValidBackupFormat("zip") {
		t.Errorf("ValidBackupFormat accepted invalid format")
	}

	if DefaultFormatForKind("volume") != "tar" || DefaultFormatForKind("dir") != "tar" || DefaultFormatForKind("panel") != "sqlite" || DefaultFormatForKind("database") != "" {
		t.Errorf("unexpected DefaultFormatForKind results")
	}

	kindFmtCases := []struct {
		kind, format string
		want         bool
	}{
		{"database", "pgdump", true},
		{"database", "dump", true},
		{"database", "sql", true},
		{"database", "archive", true},
		{"database", "rdb", true},
		{"database", "tar", false},
		{"volume", "tar", true},
		{"volume", "tgz", true},
		{"volume", "sql", false},
		{"dir", "tar", true},
		{"dir", "sqlite", false},
		{"panel", "sqlite", true},
		{"panel", "db", true},
		{"panel", "tar", false},
		{"unknown", "tar", false},
	}
	for _, tc := range kindFmtCases {
		if got := ValidKindFormat(tc.kind, tc.format); got != tc.want {
			t.Errorf("ValidKindFormat(%q, %q) = %v, want %v", tc.kind, tc.format, got, tc.want)
		}
	}

	if got := CompatibleEngines("sql"); strings.Join(got, ",") != "postgres,mysql,mariadb" {
		t.Errorf("CompatibleEngines(sql) = %v, want [postgres mysql mariadb]", got)
	}
	if got := CompatibleEngines("pgdump"); strings.Join(got, ",") != "postgres" {
		t.Errorf("CompatibleEngines(pgdump) = %v, want [postgres]", got)
	}
	if got := CompatibleEngines("archive"); strings.Join(got, ",") != "mongo" {
		t.Errorf("CompatibleEngines(archive) = %v, want [mongo]", got)
	}
	if got := CompatibleEngines("rdb"); strings.Join(got, ",") != "redis" {
		t.Errorf("CompatibleEngines(rdb) = %v, want [redis]", got)
	}
	if got := CompatibleEngines("tar"); len(got) != 0 {
		t.Errorf("CompatibleEngines(tar) = %v, want empty", got)
	}

	engCases := []struct {
		format, engine string
		want           bool
	}{
		{"", "postgres", true},
		{"any", "mysql", true},
		{"pgdump", "postgres", true},
		{"dump", "postgres", true},
		{"pgdump", "mysql", false},
		{"sql", "postgres", true},
		{"sql", "mysql", true},
		{"sql", "mariadb", true},
		{"sql", "mongo", false},
		{"archive", "mongo", true},
		{"archive", "postgres", false},
		{"rdb", "redis", true},
		{"rdb", "mongo", false},
		{"tar", "postgres", false},
	}
	for _, tc := range engCases {
		if got := FormatCompatibleWithEngine(tc.format, tc.engine); got != tc.want {
			t.Errorf("FormatCompatibleWithEngine(%q, %q) = %v, want %v", tc.format, tc.engine, got, tc.want)
		}
	}
}
