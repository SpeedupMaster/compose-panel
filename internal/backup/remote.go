package backup

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// ---------- SFTP ----------

type SFTP struct {
	addr string
	cfg  *ssh.ClientConfig
	root string
}

func NewSFTP(c TargetConfig) (*SFTP, error) {
	if c.Host == "" || c.Username == "" {
		return nil, fmt.Errorf("主机和用户名不能为空")
	}
	if c.Password == "" && c.PrivateKey == "" {
		return nil, fmt.Errorf("请填写密码或私钥")
	}
	port := c.Port
	if port == 0 {
		port = 22
	}
	var auths []ssh.AuthMethod
	if c.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(c.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("私钥无效：%w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if c.Password != "" {
		auths = append(auths, ssh.Password(c.Password))
	}
	pin := strings.TrimSpace(c.HostKey)
	hostKey := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if pin == "" {
			return nil
		}
		if fp := ssh.FingerprintSHA256(key); fp != pin && strings.TrimPrefix(fp, "SHA256:") != pin {
			return fmt.Errorf("主机指纹不匹配：服务器为 %s", fp)
		}
		return nil
	}
	root := c.Path
	if root == "" {
		root = "."
	}
	return &SFTP{
		addr: net.JoinHostPort(c.Host, strconv.Itoa(port)),
		cfg:  &ssh.ClientConfig{User: c.Username, Auth: auths, HostKeyCallback: hostKey, Timeout: 15 * time.Second},
		root: root,
	}, nil
}

type sftpConn struct {
	ssh *ssh.Client
	*sftp.Client
}

func (s *sftpConn) Close() error {
	s.Client.Close()
	return s.ssh.Close()
}

func (s *SFTP) dial(ctx context.Context) (*sftpConn, error) {
	d := net.Dialer{Timeout: 15 * time.Second}
	nc, err := d.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(nc, s.addr, s.cfg)
	if err != nil {
		nc.Close()
		return nil, err
	}
	sc := ssh.NewClient(c, chans, reqs)
	cl, err := sftp.NewClient(sc, sftp.UseConcurrentWrites(true))
	if err != nil {
		sc.Close()
		return nil, err
	}
	return &sftpConn{ssh: sc, Client: cl}, nil
}

func (s *SFTP) path(key string) string { return path.Join(s.root, path.Clean("/"+key)) }

func (s *SFTP) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	c, err := s.dial(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	p := s.path(key)
	if err := c.MkdirAll(path.Dir(p)); err != nil {
		return 0, err
	}
	tmp := p + ".part"
	f, err := c.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := f.ReadFrom(r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = c.Remove(tmp)
		return n, err
	}
	if err := c.PosixRename(tmp, p); err != nil {
		_ = c.Remove(p)
		if err := c.Rename(tmp, p); err != nil {
			return n, err
		}
	}
	return n, nil
}

type sftpReader struct {
	*sftp.File
	c *sftpConn
}

func (r *sftpReader) Close() error {
	r.File.Close()
	return r.c.Close()
}

func (s *SFTP) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	c, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	f, err := c.Open(s.path(key))
	if err != nil {
		c.Close()
		return nil, err
	}
	return &sftpReader{File: f, c: c}, nil
}

func (s *SFTP) Delete(ctx context.Context, key string) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Remove(s.path(key)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *SFTP) Test(ctx context.Context) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.MkdirAll(s.root); err != nil {
		return fmt.Errorf("无法创建目录 %s：%w", s.root, err)
	}
	p := path.Join(s.root, ".panel-write-test")
	f, err := c.Create(p)
	if err != nil {
		return fmt.Errorf("目录不可写：%w", err)
	}
	_, _ = f.Write([]byte("ok"))
	f.Close()
	return c.Remove(p)
}

func sftpRelKey(root, cur string) string {
	rootClean := path.Clean(root)
	curClean := path.Clean(cur)
	if curClean == rootClean {
		return ""
	}
	switch rootClean {
	case ".", "":
		return strings.TrimPrefix(curClean, "./")
	case "/":
		return strings.TrimPrefix(curClean, "/")
	default:
		rootPrefix := strings.TrimRight(rootClean, "/") + "/"
		if !strings.HasPrefix(curClean, rootPrefix) {
			return ""
		}
		return strings.TrimPrefix(curClean, rootPrefix)
	}
}

func (s *SFTP) List(ctx context.Context) ([]ObjectInfo, error) {
	c, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if _, err := c.Stat(s.root); err != nil {
		if os.IsNotExist(err) {
			return []ObjectInfo{}, nil
		}
		return nil, err
	}
	out := []ObjectInfo{}
	w := c.Walk(s.root)
	for w.Step() {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if err := w.Err(); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return out, err
		}
		fi := w.Stat()
		if fi == nil {
			continue
		}
		rel := sftpRelKey(s.root, w.Path())
		if rel == "" {
			continue
		}
		if fi.IsDir() {
			if strings.HasPrefix(fi.Name(), ".") || strings.Count(rel, "/")+1 > MaxListDepth {
				w.SkipDir()
			}
			continue
		}
		if IsIgnoredObjectKey(rel) || strings.Count(rel, "/") > MaxListDepth {
			continue
		}
		if len(out) >= MaxListItems {
			return out, ErrListTruncated
		}
		out = append(out, ObjectInfo{Key: rel, Size: fi.Size(), ModTime: fi.ModTime().UTC()})
	}
	return out, nil
}

// ---------- WebDAV ----------

type WebDAV struct {
	base       *url.URL
	user, pass string
	hc         *http.Client
}

func NewWebDAV(c TargetConfig) (*WebDAV, error) {
	u, err := url.Parse(strings.TrimSpace(c.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("WebDAV 地址无效")
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	return &WebDAV{base: u, user: c.Username, pass: c.Password, hc: &http.Client{Transport: sharedHTTPTransport(c.Insecure)}}, nil
}

func (d *WebDAV) url(key string) string {
	u := *d.base
	var parts []string
	for _, p := range strings.Split(strings.Trim(path.Clean("/"+key), "/"), "/") {
		if p != "" {
			parts = append(parts, url.PathEscape(p))
		}
	}
	u.RawPath = d.base.EscapedPath() + strings.Join(parts, "/")
	u.Path, _ = url.PathUnescape(u.RawPath)
	return u.String()
}

func (d *WebDAV) do(ctx context.Context, method, u string, body io.Reader, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if d.user != "" || d.pass != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(d.user+":"+d.pass)))
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return d.hc.Do(req)
}

func davErr(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
	resp.Body.Close()
	msg := strings.TrimSpace(string(b))
	if resp.StatusCode == 401 {
		msg = "认证失败"
	}
	return fmt.Errorf("WebDAV %d %s", resp.StatusCode, msg)
}

// mkcols creates each parent collection of key.
func (d *WebDAV) mkcols(ctx context.Context, key string) error {
	dirs := strings.Split(strings.Trim(path.Dir(path.Clean("/"+key)), "/"), "/")
	cur := ""
	for _, p := range dirs {
		if p == "" {
			continue
		}
		cur += p + "/"
		resp, err := d.do(ctx, "MKCOL", d.url(cur)+"/", nil, nil)
		if err != nil {
			return err
		}
		// 201 created, 405 already exists
		if resp.StatusCode != 201 && resp.StatusCode != 405 && resp.StatusCode != 301 && resp.StatusCode != 200 {
			return davErr(resp)
		}
		resp.Body.Close()
	}
	return nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (d *WebDAV) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if err := d.mkcols(ctx, key); err != nil {
		return 0, err
	}
	cr := &countReader{r: r}
	resp, err := d.do(ctx, "PUT", d.url(key), cr, map[string]string{"Content-Type": "application/octet-stream"})
	if err != nil {
		return cr.n, err
	}
	if resp.StatusCode >= 300 {
		return cr.n, davErr(resp)
	}
	resp.Body.Close()
	return cr.n, nil
}

func (d *WebDAV) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := d.do(ctx, "GET", d.url(key), nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, davErr(resp)
	}
	return resp.Body, nil
}

func (d *WebDAV) Delete(ctx context.Context, key string) error {
	resp, err := d.do(ctx, "DELETE", d.url(key), nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 && resp.StatusCode != 404 {
		return davErr(resp)
	}
	resp.Body.Close()
	return nil
}

func (d *WebDAV) Test(ctx context.Context) error {
	resp, err := d.do(ctx, "PROPFIND", d.base.String(), nil, map[string]string{"Depth": "0"})
	if err != nil {
		return err
	}
	if resp.StatusCode == 404 {
		resp.Body.Close()
		resp, err = d.do(ctx, "MKCOL", d.base.String(), nil, nil)
		if err != nil {
			return err
		}
	}
	if resp.StatusCode >= 400 {
		return davErr(resp)
	}
	resp.Body.Close()
	if _, err := d.Put(ctx, ".panel-write-test", bytes.NewReader([]byte("ok"))); err != nil {
		return fmt.Errorf("写入失败：%w", err)
	}
	return d.Delete(ctx, ".panel-write-test")
}

type davMultistatus struct {
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href      string        `xml:"href"`
	Propstats []davPropstat `xml:"propstat"`
}

type davPropstat struct {
	Prop davProp `xml:"prop"`
}

type davProp struct {
	ResourceType struct {
		Collection *struct{} `xml:"collection"`
	} `xml:"resourcetype"`
	ContentLength string `xml:"getcontentlength"`
	LastModified  string `xml:"getlastmodified"`
}

func (d *WebDAV) List(ctx context.Context) ([]ObjectInfo, error) {
	type dirItem struct {
		rel   string
		depth int
	}
	basePath := strings.TrimRight(d.base.Path, "/") + "/"
	queue := []dirItem{{rel: "", depth: 0}}
	visited := map[string]bool{"": true}
	out := []ObjectInfo{}
	propBody := `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/><d:getcontentlength/><d:getlastmodified/></d:prop></d:propfind>`

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if err := ctx.Err(); err != nil {
			return out, err
		}
		targetURL := d.base.String()
		if cur.rel != "" {
			targetURL = d.url(cur.rel) + "/"
		}
		resp, err := d.do(ctx, "PROPFIND", targetURL, strings.NewReader(propBody), map[string]string{
			"Depth":        "1",
			"Content-Type": "application/xml; charset=utf-8",
		})
		if err != nil {
			return out, err
		}
		if resp.StatusCode == http.StatusNotFound && cur.rel == "" {
			resp.Body.Close()
			return []ObjectInfo{}, nil
		}
		if resp.StatusCode >= 300 {
			return out, davErr(resp)
		}
		var ms davMultistatus
		decErr := xml.NewDecoder(resp.Body).Decode(&ms)
		resp.Body.Close()
		if decErr != nil {
			return out, fmt.Errorf("解析 WebDAV 目录响应失败：%w", decErr)
		}

		for _, r := range ms.Responses {
			href := strings.TrimSpace(r.Href)
			if href == "" {
				continue
			}
			hrefPath := href
			if u, err := url.Parse(href); err == nil && u.Path != "" {
				hrefPath = u.Path
			} else if dec, err := url.PathUnescape(href); err == nil {
				hrefPath = dec
			}
			isTrailingSlash := strings.HasSuffix(hrefPath, "/")
			cleanHref := path.Clean("/" + hrefPath)
			cleanBase := strings.TrimRight(path.Clean("/"+basePath), "/")
			var rel string
			if cleanHref == cleanBase {
				continue
			}
			if cleanBase == "" || cleanBase == "/" {
				rel = strings.TrimLeft(cleanHref, "/")
			} else if strings.HasPrefix(cleanHref, cleanBase+"/") {
				rel = strings.TrimPrefix(cleanHref, cleanBase+"/")
			} else {
				continue
			}
			rel = strings.Trim(rel, "/")
			if rel == "" || rel == cur.rel {
				continue
			}

			isCol := false
			var size int64 = -1
			var modTime time.Time
			for _, ps := range r.Propstats {
				if ps.Prop.ResourceType.Collection != nil {
					isCol = true
				}
				if s := strings.TrimSpace(ps.Prop.ContentLength); s != "" {
					if n, err := strconv.ParseInt(s, 10, 64); err == nil {
						size = n
					}
				}
				if lm := strings.TrimSpace(ps.Prop.LastModified); lm != "" {
					if t, err := http.ParseTime(lm); err == nil {
						modTime = t.UTC()
					} else if t, err := time.Parse(time.RFC3339, lm); err == nil {
						modTime = t.UTC()
					}
				}
			}
			if !isCol && isTrailingSlash && size <= 0 {
				isCol = true
			}
			if isCol {
				if !strings.HasPrefix(path.Base(rel), ".") && cur.depth+1 <= MaxListDepth && !visited[rel] {
					visited[rel] = true
					queue = append(queue, dirItem{rel: rel, depth: cur.depth + 1})
				}
				continue
			}
			if IsIgnoredObjectKey(rel) || strings.Count(rel, "/") > MaxListDepth {
				continue
			}
			if len(out) >= MaxListItems {
				return out, ErrListTruncated
			}
			out = append(out, ObjectInfo{Key: rel, Size: size, ModTime: modTime})
		}
	}
	return out, nil
}
