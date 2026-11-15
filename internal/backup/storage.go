// Package backup streams dumps through compression and encryption into storage targets.
package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Storage is a place where backup objects live.
type Storage interface {
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	Test(ctx context.Context) error
}

// TargetConfig is the decrypted JSON stored in storage_targets.config.
type TargetConfig struct {
	Path      string `json:"path,omitempty"` // local
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	AccessKey string `json:"accessKey,omitempty"`
	SecretKey string `json:"secretKey,omitempty"`
	Insecure  bool   `json:"insecure,omitempty"` // http instead of https
	PathStyle bool   `json:"pathStyle,omitempty"`
}

func ParseTargetConfig(s string) (TargetConfig, error) {
	var c TargetConfig
	if s == "" {
		return c, nil
	}
	err := json.Unmarshal([]byte(s), &c)
	return c, err
}

func Open(kind string, c TargetConfig) (Storage, error) {
	switch kind {
	case "local":
		if c.Path == "" {
			return nil, fmt.Errorf("本地路径不能为空")
		}
		return &Local{Root: c.Path}, nil
	case "s3":
		return NewS3(c)
	}
	return nil, fmt.Errorf("unknown storage kind %q", kind)
}

// Local stores objects as files under Root.
type Local struct{ Root string }

func (l *Local) path(key string) (string, error) {
	clean := filepath.Clean("/" + key)
	if strings.Contains(clean, "..") {
		return "", fmt.Errorf("invalid key")
	}
	return filepath.Join(l.Root, clean), nil
}

func (l *Local) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	p, err := l.path(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return 0, err
	}
	tmp := p + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return n, err
	}
	return n, os.Rename(tmp, p)
}

func (l *Local) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (l *Local) Delete(ctx context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (l *Local) Test(ctx context.Context) error {
	if err := os.MkdirAll(l.Root, 0o750); err != nil {
		return err
	}
	p := filepath.Join(l.Root, ".panel-write-test")
	if err := os.WriteFile(p, []byte(time.Now().String()), 0o640); err != nil {
		return fmt.Errorf("目录不可写：%w", err)
	}
	return os.Remove(p)
}

// S3 stores objects in an S3-compatible bucket.
type S3 struct {
	c      *minio.Client
	bucket string
	prefix string
}

func NewS3(c TargetConfig) (*S3, error) {
	ep := strings.TrimPrefix(strings.TrimPrefix(c.Endpoint, "https://"), "http://")
	ep = strings.TrimRight(ep, "/")
	if ep == "" || c.Bucket == "" {
		return nil, fmt.Errorf("Endpoint 和 Bucket 不能为空")
	}
	secure := !c.Insecure && !strings.HasPrefix(c.Endpoint, "http://")
	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure: secure,
		Region: c.Region,
	}
	if c.PathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}
	cl, err := minio.New(ep, opts)
	if err != nil {
		return nil, err
	}
	return &S3{c: cl, bucket: c.Bucket, prefix: strings.Trim(c.Prefix, "/")}, nil
}

func (s *S3) key(k string) string {
	if s.prefix == "" {
		return k
	}
	return s.prefix + "/" + k
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	info, err := s.c.PutObject(ctx, s.bucket, s.key(key), r, -1, minio.PutObjectOptions{ContentType: "application/octet-stream", PartSize: 16 << 20})
	if err != nil {
		return 0, err
	}
	return info.Size, nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.c.GetObject(ctx, s.bucket, s.key(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, err
	}
	return obj, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	return s.c.RemoveObject(ctx, s.bucket, s.key(key), minio.RemoveObjectOptions{})
}

func (s *S3) Test(ctx context.Context) error {
	ok, err := s.c.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("Bucket %s 不存在", s.bucket)
	}
	k := s.key(".panel-write-test")
	if _, err := s.c.PutObject(ctx, s.bucket, k, strings.NewReader("ok"), 2, minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("写入失败：%w", err)
	}
	return s.c.RemoveObject(ctx, s.bucket, k, minio.RemoveObjectOptions{})
}
