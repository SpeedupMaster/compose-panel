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
	"os/exec"
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
		zw, err := zstd.NewWriter(sink, zstd.WithEncoderLevel(zstd.SpeedDefault))
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
		zr, err := zstd.NewReader(br)
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

// CmdReader runs cmd and returns its stdout as a stream; Close waits for the process
// and reports its stderr on failure.
func CmdReader(cmd *exec.Cmd, log io.Writer) (io.ReadCloser, error) {
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	tail := &tailBuf{max: 4000}
	if log != nil {
		cmd.Stderr = io.MultiWriter(tail, log)
	} else {
		cmd.Stderr = tail
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &cmdStream{ReadCloser: out, cmd: cmd, tail: tail}, nil
}

type cmdStream struct {
	io.ReadCloser
	cmd    *exec.Cmd
	tail   *tailBuf
	err    error
	waited bool
}

func (c *cmdStream) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if err == io.EOF {
		if werr := c.wait(); werr != nil {
			return n, werr
		}
	}
	return n, err
}

func (c *cmdStream) wait() error {
	if c.waited {
		return c.err
	}
	c.waited = true
	if err := c.cmd.Wait(); err != nil {
		msg := strings.TrimSpace(c.tail.String())
		if msg == "" {
			msg = err.Error()
		}
		c.err = fmt.Errorf("%s", msg)
	}
	return c.err
}

func (c *cmdStream) Close() error {
	c.ReadCloser.Close()
	return c.wait()
}

type tailBuf struct {
	b   []byte
	max int
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuf) String() string { return string(t.b) }

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

// UntarDir extracts a tar stream into dir, refusing paths that escape it.
func UntarDir(r io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
		target := filepath.Join(dir, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) && target != filepath.Clean(dir) {
			return fmt.Errorf("非法路径：%s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		case tar.TypeSymlink:
			_ = os.Symlink(hdr.Linkname, target)
		}
	}
}
