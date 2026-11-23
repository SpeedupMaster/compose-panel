package backup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
)

// Result describes a stored object.
type Result struct {
	Size   int64
	SHA256 string
}

type countingHash struct {
	n int64
	h interface {
		io.Writer
		Sum([]byte) []byte
	}
}

func (c *countingHash) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return c.h.Write(p)
}

// ParseRecipients parses age public keys (one per entry, blanks ignored).
func ParseRecipients(keys ...string) ([]age.Recipient, error) {
	var out []age.Recipient
	for _, k := range keys {
		for _, line := range strings.Split(k, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			r, err := age.ParseX25519Recipient(line)
			if err != nil {
				return nil, fmt.Errorf("age 公钥无效：%w", err)
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// NewIdentity creates an age keypair, returning (identity, recipient).
func NewIdentity() (string, string, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", err
	}
	return id.String(), id.Recipient().String(), nil
}

// Store compresses src with zstd, optionally encrypts it with age and uploads it to key.
// The checksum covers the stored bytes.
func Store(ctx context.Context, st Storage, key string, src io.Reader, recipients []age.Recipient) (Result, error) {
	pr, pw := io.Pipe()
	sum := &countingHash{h: sha256.New()}
	go func() {
		out := io.MultiWriter(pw, sum)
		var sink io.WriteCloser = nopCloser{out}
		if len(recipients) > 0 {
			aw, err := age.Encrypt(out, recipients...)
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			sink = aw
		}
		zw, err := zstd.NewWriter(sink,
			zstd.WithEncoderLevel(zstd.SpeedDefault),
			zstd.WithEncoderConcurrency(1),
			zstd.WithLowerEncoderMem(true),
			zstd.WithWindowSize(1<<20),
		)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(zw, src); err != nil {
			zw.Close()
			pw.CloseWithError(err)
			return
		}
		if err := zw.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		if err := sink.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()
	n, err := st.Put(ctx, key, pr)
	pr.CloseWithError(err)
	if err != nil {
		return Result{}, err
	}
	if n <= 0 {
		n = sum.n
	}
	return Result{Size: sum.n, SHA256: hex.EncodeToString(sum.h.Sum(nil))}, nil
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// Verify downloads key and checks size and checksum.
func Verify(ctx context.Context, st Storage, key, wantSHA string, wantSize int64) error {
	rc, err := st.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("下载失败：%w", err)
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return fmt.Errorf("读取失败：%w", err)
	}
	if wantSize > 0 && n != wantSize {
		return fmt.Errorf("大小不一致：期望 %d，实际 %d", wantSize, n)
	}
	if got := hex.EncodeToString(h.Sum(nil)); wantSHA != "" && got != wantSHA {
		return fmt.Errorf("SHA-256 校验失败")
	}
	return nil
}

// OpenStream decrypts (when encrypted or matching age magic header) and auto-decompresses
// (zstd, gzip, or plain) rc using the supplied age identities.
func OpenStream(rc io.ReadCloser, encrypted bool, identities []string) (io.ReadCloser, error) {
	br := bufio.NewReaderSize(rc, 64<<10)
	head, _ := br.Peek(len(ageMagic))
	if encrypted || HasAgeHeader(head) {
		var ids []age.Identity
		for _, raw := range identities {
			s := strings.TrimSpace(raw)
			if s == "" {
				continue
			}
			id, err := age.ParseX25519Identity(s)
			if err != nil {
				continue
			}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			rc.Close()
			return nil, fmt.Errorf("备份已加密，但未找到可用的解密私钥")
		}
		dr, err := age.Decrypt(br, ids...)
		if err != nil {
			rc.Close()
			return nil, fmt.Errorf("解密失败（面板密钥与已保存的历史私钥均不匹配，请在恢复时提供原备份私钥）：%w", err)
		}
		br = bufio.NewReaderSize(dr, 64<<10)
	}

	magic, _ := br.Peek(4)
	switch {
	case bytes.HasPrefix(magic, zstdMagic):
		zr, err := zstd.NewReader(br,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderLowmem(true),
		)
		if err != nil {
			rc.Close()
			return nil, err
		}
		return &stackCloser{Reader: zr.IOReadCloser(), closers: []io.Closer{rc}}, nil
	case bytes.HasPrefix(magic, gzipMagic):
		gr, err := gzip.NewReader(br)
		if err != nil {
			rc.Close()
			return nil, fmt.Errorf("gzip 解压失败：%w", err)
		}
		return &stackCloser{Reader: gr, closers: []io.Closer{rc}}, nil
	default:
		return &stackCloser{Reader: br, closers: []io.Closer{rc}}, nil
	}
}

// OpenObject returns the plaintext stream of a stored object, auto-detecting age encryption and compression.
func OpenObject(ctx context.Context, st Storage, key string, encrypted bool, identities ...string) (io.ReadCloser, error) {
	rc, err := st.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return OpenStream(rc, encrypted, identities)
}

type stackCloser struct {
	io.Reader
	closers []io.Closer
}

func (s *stackCloser) Close() error {
	if c, ok := s.Reader.(io.Closer); ok {
		c.Close()
	}
	for _, c := range s.closers {
		c.Close()
	}
	return nil
}

// TarDir streams a tar of dir.
func TarDir(dir string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			if rel == "." {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			link := ""
			if info.Mode()&os.ModeSymlink != 0 {
				link, _ = os.Readlink(p)
			}
			hdr, err := tar.FileInfoHeader(info, link)
			if err != nil {
				return err
			}
			hdr.Name = filepath.ToSlash(rel)
			if d.IsDir() {
				hdr.Name += "/"
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				f, err := os.Open(p)
				if err != nil {
					return err
				}
				_, err = io.Copy(tw, f)
				f.Close()
				return err
			}
			return nil
		})
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	return pr
}

func withinDir(rootClean, candidate string) bool {
	c := filepath.Clean(candidate)
	if c == rootClean {
		return true
	}
	sep := string(filepath.Separator)
	pfx := rootClean
	if !strings.HasSuffix(pfx, sep) {
		pfx += sep
	}
	return strings.HasPrefix(c, pfx)
}

func ensureNoSymlinkParent(rootClean, target string) error {
	rel, err := filepath.Rel(rootClean, filepath.Dir(target))
	if err != nil || rel == "." || rel == "" {
		return nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("非法路径：%s", target)
	}
	cur := rootClean
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if seg == "" || seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("非法符号链接路径：%s", seg)
		}
	}
	return nil
}

// UntarDir extracts a tar stream into dir, refusing paths, hardlinks, or symlinks that escape dir.
func UntarDir(r io.Reader, dir string) error {
	cleanDir := filepath.Clean(dir)
	if err := os.MkdirAll(cleanDir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(cleanDir, filepath.FromSlash(hdr.Name))
		if !withinDir(cleanDir, target) {
			return fmt.Errorf("非法路径：%s", hdr.Name)
		}
		if err := ensureNoSymlinkParent(cleanDir, target); err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("非法符号链接目录：%s", hdr.Name)
			}
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("非法符号链接文件：%s", hdr.Name)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			link := strings.TrimSpace(hdr.Linkname)
			if link == "" || filepath.IsAbs(link) || strings.HasPrefix(link, "/") || strings.HasPrefix(link, `\`) {
				return fmt.Errorf("非法符号链接目标：%s -> %s", hdr.Name, hdr.Linkname)
			}
			resolved := filepath.Join(filepath.Dir(target), filepath.FromSlash(link))
			if !withinDir(cleanDir, resolved) {
				return fmt.Errorf("符号链接超出目标目录：%s -> %s", hdr.Name, hdr.Linkname)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(link, target); err != nil {
				return fmt.Errorf("创建符号链接失败：%w", err)
			}
		case tar.TypeLink:
			return fmt.Errorf("不支持包含硬链接的归档：%s", hdr.Name)
		}
	}
}
