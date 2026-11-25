package backup

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestStoreZstdBoundedWindowAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := &Local{Root: dir}
	// > 2 MiB payload so zstd emits a multi-block frame with explicit Window_Descriptor.
	payload := bytes.Repeat([]byte("compose-panel backup row 0123456789abcdef\n"), 60000)

	res, err := Store(context.Background(), st, "db/test.sql.zst", bytes.NewReader(payload), nil)
	if err != nil {
		t.Fatalf("Store() error: %v", err)
	}
	if res.Size <= 0 || res.SHA256 == "" {
		t.Fatalf("unexpected Store result: %+v", res)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "db", "test.sql.zst"))
	if err != nil {
		t.Fatalf("read stored file: %v", err)
	}
	var hdr zstd.Header
	if err := hdr.Decode(raw); err != nil {
		t.Fatalf("decode zstd header: %v", err)
	}
	if hdr.WindowSize > 1<<20 {
		t.Fatalf("zstd frame WindowSize = %d bytes, want <= %d (1 MiB)", hdr.WindowSize, 1<<20)
	}

	// Round-trip decompression through OpenObject.
	rc, err := OpenObject(context.Background(), st, "db/test.sql.zst", false)
	if err != nil {
		t.Fatalf("OpenObject() error: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("read OpenObject: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("round-trip mismatch: got %d bytes, want %d", len(got), len(payload))
	}

	// Encrypted round-trip + legacy 8 MiB-window zstd compatibility.
	id, rcp, err := NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	rcps, err := ParseRecipients(rcp)
	if err != nil {
		t.Fatalf("ParseRecipients: %v", err)
	}
	if _, err := Store(context.Background(), st, "db/enc.sql.zst.age", bytes.NewReader(payload[:128<<10]), rcps); err != nil {
		t.Fatalf("Store(encrypted) error: %v", err)
	}
	erc, err := OpenObject(context.Background(), st, "db/enc.sql.zst.age", true, id)
	if err != nil {
		t.Fatalf("OpenObject(encrypted) error: %v", err)
	}
	egot, err := io.ReadAll(erc)
	erc.Close()
	if err != nil || !bytes.Equal(egot, payload[:128<<10]) {
		t.Fatalf("encrypted round-trip failed: err=%v len=%d", err, len(egot))
	}

	// OpenStream must still read older 8 MiB-window archives.
	var legacy bytes.Buffer
	zw, err := zstd.NewWriter(&legacy, zstd.WithWindowSize(8<<20))
	if err != nil {
		t.Fatalf("legacy zstd writer: %v", err)
	}
	_, _ = zw.Write(payload[:256<<10])
	_ = zw.Close()
	lrc, err := OpenStream(io.NopCloser(bytes.NewReader(legacy.Bytes())), false, nil)
	if err != nil {
		t.Fatalf("OpenStream(legacy) error: %v", err)
	}
	lgot, err := io.ReadAll(lrc)
	lrc.Close()
	if err != nil || !bytes.Equal(lgot, payload[:256<<10]) {
		t.Fatalf("legacy zstd read failed: err=%v len=%d", err, len(lgot))
	}
}

func TestStorageTransportReuseAndChunkLimits(t *testing.T) {
	d1, err := NewWebDAV(TargetConfig{URL: "https://dav.example.com/root/"})
	if err != nil {
		t.Fatalf("NewWebDAV(1): %v", err)
	}
	d2, err := NewWebDAV(TargetConfig{URL: "https://dav.example.com/other/"})
	if err != nil {
		t.Fatalf("NewWebDAV(2): %v", err)
	}
	if d1.hc.Transport != d2.hc.Transport {
		t.Fatalf("NewWebDAV created distinct transports (%p vs %p), want shared transport", d1.hc.Transport, d2.hc.Transport)
	}

	di1, err := NewWebDAV(TargetConfig{URL: "https://dav.example.com/root/", Insecure: true})
	if err != nil {
		t.Fatalf("NewWebDAV(insecure 1): %v", err)
	}
	di2, err := NewWebDAV(TargetConfig{URL: "https://dav.example.com/other/", Insecure: true})
	if err != nil {
		t.Fatalf("NewWebDAV(insecure 2): %v", err)
	}
	if di1.hc.Transport != di2.hc.Transport {
		t.Fatalf("NewWebDAV(insecure) created distinct transports, want shared insecure transport")
	}
	if d1.hc.Transport == di1.hc.Transport {
		t.Fatalf("secure and insecure WebDAV must not share the same transport")
	}

	if defaultGDriveChunkSize > 4<<20 {
		t.Fatalf("defaultGDriveChunkSize = %d, want <= %d (4 MiB)", defaultGDriveChunkSize, 4<<20)
	}
	if s3PartSize > 8<<20 {
		t.Fatalf("s3PartSize = %d, want <= %d (8 MiB)", s3PartSize, 8<<20)
	}
	opts := s3PutOptions()
	if opts.NumThreads != 1 {
		t.Fatalf("s3PutOptions().NumThreads = %d, want 1 (to bound streaming upload memory)", opts.NumThreads)
	}
	if opts.PartSize != s3PartSize {
		t.Fatalf("s3PutOptions().PartSize = %d, want %d", opts.PartSize, s3PartSize)
	}
}

func TestOpenStreamRejectsOversizedZstdWindow(t *testing.T) {
	// Craft a minimal valid zstd frame declaring a 64 MiB window (> 32 MiB cap):
	// Magic: 0xFD2FB528 (LE: 28 B5 2F FD)
	// Frame_Header_Descriptor: 0x00 (Single_Segment_Flag=0 -> Window_Descriptor follows, no dict/checksum/FCS)
	// Window_Descriptor: exponent=16, mantissa=0 -> (1 << (10 + 16)) = 64 MiB -> byte = (16 << 3) | 0 = 0x80
	// Final empty raw block header (3 bytes LE): Last_Block=1, Block_Type=0, Block_Size=0 -> 01 00 00
	frame64MiB := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x80, 0x01, 0x00, 0x00}

	var hdr zstd.Header
	if err := hdr.Decode(frame64MiB); err != nil {
		t.Fatalf("crafted header invalid: %v", err)
	}
	if hdr.WindowSize != 64<<20 {
		t.Fatalf("crafted WindowSize = %d, want %d", hdr.WindowSize, 64<<20)
	}

	rc, err := OpenStream(io.NopCloser(bytes.NewReader(frame64MiB)), false, nil)
	if err == nil {
		_, err = io.ReadAll(rc)
		rc.Close()
	}
	if err == nil {
		t.Fatalf("OpenStream accepted 64 MiB zstd window, want rejection above %d bytes", zstdDecoderMaxWindow)
	}
}
