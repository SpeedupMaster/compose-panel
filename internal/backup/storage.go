// Package backup streams dumps through compression and encryption into storage targets.
package backup

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	MaxListItems = 5000
	MaxListDepth = 6
	s3PartSize   = 8 << 20 // 8 MiB per multipart chunk (supports up to ~80 GiB objects)
)

var (
	sharedTransportOnce     sync.Once
	sharedSecureTransport   *http.Transport
	sharedInsecureTransport *http.Transport
)

func sharedHTTPTransport(insecure bool) *http.Transport {
	sharedTransportOnce.Do(func() {
		sec := http.DefaultTransport.(*http.Transport).Clone()
		sec.MaxIdleConns = 16
		sec.MaxIdleConnsPerHost = 4
		sec.IdleConnTimeout = 30 * time.Second
		sharedSecureTransport = sec

		ins := sec.Clone()
		ins.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		sharedInsecureTransport = ins
	})
	if insecure {
		return sharedInsecureTransport
	}
	return sharedSecureTransport
}

// ErrListTruncated is returned alongside partial results when List reaches MaxListItems.
var ErrListTruncated = errors.New("对象数量超过扫描上限")

// ObjectInfo describes one object found in a storage target.
type ObjectInfo struct {
	Key     string    `json:"key"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

// Storage is a place where backup objects live.
type Storage interface {
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	Test(ctx context.Context) error
	List(ctx context.Context) ([]ObjectInfo, error)
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
	// sftp / webdav (Path is the remote directory for sftp)
	Host       string `json:"host,omitempty"`
	Port       int    `json:"port,omitempty"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"privateKey,omitempty"`
	HostKey    string `json:"hostKey,omitempty"` // optional SHA256 fingerprint pin
	URL        string `json:"url,omitempty"`     // webdav collection URL
	// gdrive (personal Google Drive via OAuth 2.0; reuses Prefix for optional subfolder path)
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	FolderID     string `json:"folderId,omitempty"`
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
	case "sftp":
		return NewSFTP(c)
	case "webdav":
		return NewWebDAV(c)
	case "gdrive":
		return NewGDrive(c)
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

func (l *Local) List(ctx context.Context) ([]ObjectInfo, error) {
	if _, err := os.Stat(l.Root); err != nil {
		if os.IsNotExist(err) {
			return []ObjectInfo{}, nil
		}
		return nil, err
	}
	out := []ObjectInfo{}
	truncated := false
	errStop := errors.New("stop")
	err := filepath.WalkDir(l.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(l.Root, p)
		if err != nil || rel == "." {
			return nil
		}
		slash := filepath.ToSlash(rel)
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			if strings.Count(slash, "/")+1 > MaxListDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if IsIgnoredObjectKey(slash) || strings.Count(slash, "/") > MaxListDepth {
			return nil
		}
		if len(out) >= MaxListItems {
			truncated = true
			return errStop
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		out = append(out, ObjectInfo{Key: slash, Size: info.Size(), ModTime: info.ModTime().UTC()})
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return out, err
	}
	if truncated {
		return out, ErrListTruncated
	}
	return out, nil
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
		Creds:     credentials.NewStaticV4(c.AccessKey, c.SecretKey, ""),
		Secure:    secure,
		Region:    c.Region,
		Transport: sharedHTTPTransport(false),
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

func s3PutOptions() minio.PutObjectOptions {
	return minio.PutObjectOptions{
		ContentType: "application/octet-stream",
		PartSize:    s3PartSize,
		NumThreads:  1,
	}
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	info, err := s.c.PutObject(ctx, s.bucket, s.key(key), r, -1, s3PutOptions())
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

func (s *S3) List(ctx context.Context) ([]ObjectInfo, error) {
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pfx := s.prefix
	if pfx != "" && !strings.HasSuffix(pfx, "/") {
		pfx += "/"
	}
	out := []ObjectInfo{}
	for obj := range s.c.ListObjects(lctx, s.bucket, minio.ListObjectsOptions{Prefix: pfx, Recursive: true}) {
		if obj.Err != nil {
			return out, obj.Err
		}
		rel := strings.TrimPrefix(obj.Key, pfx)
		rel = strings.TrimLeft(rel, "/")
		if strings.HasSuffix(rel, "/") || IsIgnoredObjectKey(rel) || strings.Count(rel, "/") > MaxListDepth {
			continue
		}
		if len(out) >= MaxListItems {
			cancel()
			return out, ErrListTruncated
		}
		out = append(out, ObjectInfo{Key: rel, Size: obj.Size, ModTime: obj.LastModified.UTC()})
	}
	return out, nil
}
