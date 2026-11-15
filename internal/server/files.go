package server

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FSEntry is one row of a directory listing.
type FSEntry struct {
	Name  string `json:"name"`
	Type  string `json:"type"` // dir | file | link | other
	Dir   bool   `json:"dir"`  // true for directories and links to directories
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	Mode  string `json:"mode"`
}

type fsListing struct {
	Path      string    `json:"path"`
	Entries   []FSEntry `json:"entries"`
	Truncated bool      `json:"truncated"`
	Via       string    `json:"via"` // exec | archive | helper
}

const maxListEntries = 3000

// lsScript prints "<link>|<type>|<size>|<mtime>|<mode>|<name>" per entry of $1.
// Exit 3: no such path, 4: not a directory.
const lsScript = `cd -- "$1" 2>/dev/null || { [ -e "$1" ] && exit 4; exit 3; }
n=0
for f in .[!.]* ..?* *; do
  [ -e "$f" ] || [ -L "$f" ] || continue
  n=$((n+1)); if [ $n -gt ` + "3000" + ` ]; then echo TRUNCATED; break; fi
  if [ -L "$f" ]; then if [ -d "$f" ]; then l=D; else l=L; fi; else l=-; fi
  printf '%s|' "$l"; stat -c '%F|%s|%Y|%A|%n' -- "$f" 2>/dev/null || echo "other|0|0|?|$f"
done`

var errNoShell = errors.New("no shell")

func parseLs(out []byte) ([]FSEntry, bool) {
	entries := []FSEntry{}
	trunc := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "TRUNCATED" {
			trunc = true
			continue
		}
		f := strings.SplitN(line, "|", 6)
		if len(f) != 6 {
			continue
		}
		e := FSEntry{Name: f[5], Mode: f[4]}
		e.Size, _ = strconv.ParseInt(f[2], 10, 64)
		e.Mtime, _ = strconv.ParseInt(f[3], 10, 64)
		switch {
		case f[0] == "D":
			e.Type, e.Dir = "link", true
		case f[0] == "L":
			e.Type = "link"
		case f[1] == "directory":
			e.Type, e.Dir = "dir", true
		case strings.HasPrefix(f[1], "regular"):
			e.Type = "file"
		default:
			e.Type = "other"
		}
		entries = append(entries, e)
	}
	sortEntries(entries)
	return entries, trunc
}

func sortEntries(es []FSEntry) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].Dir != es[j].Dir {
			return es[i].Dir
		}
		return strings.ToLower(es[i].Name) < strings.ToLower(es[j].Name)
	})
}

func lsError(code int, p string) error {
	switch code {
	case 3:
		return bad("路径不存在：%s", p)
	case 4:
		return bad("不是目录：%s", p)
	}
	return nil
}

func cleanAbs(p string) string {
	return path.Clean("/" + strings.ReplaceAll(p, "\\", "/"))
}

func validFileName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, "/\\\x00")
}

func attachment(w http.ResponseWriter, name string, size int64) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	if size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
}

// ---------- container files ----------

func (s *Server) handleContainerFS(w http.ResponseWriter, r *http.Request, u *User) error {
	c, err := s.containerFor(r, u)
	if err != nil {
		return err
	}
	p := cleanAbs(r.URL.Query().Get("path"))
	if c.State.Running {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", "exec", c.Id, "sh", "-c", lsScript, "sh", p).Output()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if e := lsError(ee.ExitCode(), p); e != nil {
				return e
			}
		}
		if err == nil {
			es, trunc := parseLs(out)
			return ok(w, fsListing{Path: p, Entries: es, Truncated: trunc, Via: "exec"})
		}
	}
	// Fallback for stopped containers or images without a shell: read the tar headers.
	st, err := s.dk.StatPath(r.Context(), c.Id, p)
	if err != nil {
		return bad("路径不存在：%s", p)
	}
	if !st.IsDir() {
		return bad("不是目录：%s", p)
	}
	es, trunc, err := s.listViaArchive(r.Context(), c.Id, p)
	if err != nil {
		return err
	}
	return ok(w, fsListing{Path: p, Entries: es, Truncated: trunc, Via: "archive"})
}

func (s *Server) listViaArchive(ctx context.Context, id, p string) ([]FSEntry, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rc, err := s.dk.GetArchive(ctx, id, p)
	if err != nil {
		return nil, false, err
	}
	defer rc.Close()
	counter := &countingReader{r: rc}
	tr := tar.NewReader(counter)
	entries := []FSEntry{}
	seen := map[string]bool{}
	trunc := false
	for {
		h, err := tr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			trunc = true
			break
		}
		if counter.n > 256<<20 || len(entries) >= maxListEntries {
			trunc = true
			break
		}
		name := strings.TrimSuffix(h.Name, "/")
		parts := strings.SplitN(name, "/", 3)
		if len(parts) < 2 || parts[1] == "" {
			continue // the directory itself
		}
		child := parts[1]
		if seen[child] {
			continue
		}
		seen[child] = true
		e := FSEntry{Name: child, Mtime: h.ModTime.Unix(), Mode: h.FileInfo().Mode().String()}
		if len(parts) == 3 {
			e.Type, e.Dir = "dir", true // we only saw a descendant
		} else {
			switch h.Typeflag {
			case tar.TypeDir:
				e.Type, e.Dir = "dir", true
			case tar.TypeReg:
				e.Type, e.Size = "file", h.Size
			case tar.TypeSymlink:
				e.Type = "link"
			default:
				e.Type = "other"
			}
		}
		entries = append(entries, e)
	}
	sortEntries(entries)
	return entries, trunc, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (s *Server) handleContainerDownload(w http.ResponseWriter, r *http.Request, u *User) error {
	c, err := s.containerFor(r, u)
	if err != nil {
		return err
	}
	p := cleanAbs(r.URL.Query().Get("path"))
	st, err := s.dk.StatPath(r.Context(), c.Id, p)
	if err != nil {
		return bad("路径不存在：%s", p)
	}
	rc, err := s.dk.GetArchive(r.Context(), c.Id, p)
	if err != nil {
		return err
	}
	defer rc.Close()
	cname := strings.TrimPrefix(c.Name, "/")
	s.audit(r, u, "container.download", cname, p, nil)
	if st.IsDir() {
		attachment(w, path.Base(p)+".tar", -1)
		_, _ = io.Copy(w, rc)
		return nil
	}
	tr := tar.NewReader(rc)
	for {
		h, err := tr.Next()
		if err != nil {
			return bad("读取文件失败：%v", err)
		}
		if h.Typeflag == tar.TypeReg {
			attachment(w, path.Base(h.Name), h.Size)
			_, _ = io.Copy(w, tr)
			return nil
		}
		if h.Typeflag == tar.TypeSymlink {
			return bad("这是一个符号链接，指向 %s", h.Linkname)
		}
	}
}

// handleContainerUpload writes the raw request body as dir/name inside the container.
func (s *Server) handleContainerUpload(w http.ResponseWriter, r *http.Request, u *User) error {
	c, err := s.containerFor(r, u)
	if err != nil {
		return err
	}
	dir := cleanAbs(r.URL.Query().Get("path"))
	name := r.URL.Query().Get("name")
	if !validFileName(name) {
		return bad("文件名无效")
	}
	if r.ContentLength < 0 {
		return bad("缺少 Content-Length")
	}
	if st, err := s.dk.StatPath(r.Context(), c.Id, dir); err != nil || !st.IsDir() {
		return bad("目标目录不存在：%s", dir)
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: r.ContentLength, ModTime: time.Now(), Typeflag: tar.TypeReg})
		if err == nil {
			_, err = io.CopyN(tw, r.Body, r.ContentLength)
		}
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	err = s.dk.PutArchive(r.Context(), c.Id, dir, pr)
	_ = pr.Close()
	cname := strings.TrimPrefix(c.Name, "/")
	s.audit(r, u, "container.upload", cname, path.Join(dir, name)+" ("+humanBytes(r.ContentLength)+")", err)
	return done(w, err)
}

// ---------- volume files (via a throwaway helper container) ----------

func (s *Server) volumeArg(r *http.Request) (string, error) {
	name := r.PathValue("name")
	exists, err := s.dk.VolumeExists(r.Context(), name)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", errNotFound
	}
	return name, nil
}

func volPath(p string) (rel, full string) {
	rel = cleanAbs(p)
	if rel == "/" {
		return rel, "/v"
	}
	return rel, "/v" + rel
}

func (s *Server) helperCmd(ctx context.Context, vol string, ro, stdin bool, script string, args ...string) *exec.Cmd {
	mount := vol + ":/v"
	if ro {
		mount += ":ro"
	}
	a := []string{"run", "--rm", "--network", "none"}
	if stdin {
		a = append(a, "-i")
	}
	a = append(a, "-v", mount, s.cfg.HelperImage, "sh", "-c", script, "sh")
	return exec.CommandContext(ctx, "docker", append(a, args...)...)
}

func (s *Server) handleVolumeFS(w http.ResponseWriter, r *http.Request, u *User) error {
	vol, err := s.volumeArg(r)
	if err != nil {
		return err
	}
	rel, full := volPath(r.URL.Query().Get("path"))
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := s.helperCmd(ctx, vol, true, false, lsScript, full)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if e := lsError(ee.ExitCode(), rel); e != nil {
			return e
		}
	}
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	es, trunc := parseLs(out)
	return ok(w, fsListing{Path: rel, Entries: es, Truncated: trunc, Via: "helper"})
}

func (s *Server) handleVolumeDownload(w http.ResponseWriter, r *http.Request, u *User) error {
	vol, err := s.volumeArg(r)
	if err != nil {
		return err
	}
	rel, full := volPath(r.URL.Query().Get("path"))
	script := `if [ -d "$1" ]; then echo D; tar -C "$1" -cf - .; elif [ -f "$1" ]; then echo F; cat -- "$1"; else echo N; fi`
	cmd := s.helperCmd(r.Context(), vol, true, false, script, full)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Wait()
	br := bufio.NewReader(out)
	kind, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("读取失败：%s", strings.TrimSpace(stderr.String()))
	}
	base := path.Base(rel)
	if rel == "/" {
		base = vol
	}
	switch strings.TrimSpace(kind) {
	case "D":
		attachment(w, base+".tar", -1)
	case "F":
		attachment(w, base, -1)
	default:
		_, _ = io.Copy(io.Discard, br)
		return bad("路径不存在：%s", rel)
	}
	s.audit(r, u, "volume.download", vol, rel, nil)
	_, _ = io.Copy(w, br)
	return nil
}

func (s *Server) handleVolumeUpload(w http.ResponseWriter, r *http.Request, u *User) error {
	vol, err := s.volumeArg(r)
	if err != nil {
		return err
	}
	rel, full := volPath(r.URL.Query().Get("path"))
	name := r.URL.Query().Get("name")
	if !validFileName(name) {
		return bad("文件名无效")
	}
	// Keep the owner of the target directory so the app can still read/write the file.
	script := `[ -d "$1" ] || { echo "目录不存在" >&2; exit 3; }
cat > "$1/$2" && chown "$(stat -c %u:%g "$1")" "$1/$2"`
	cmd := s.helperCmd(r.Context(), vol, false, true, script, full, name)
	cmd.Stdin = r.Body
	out, err := cmd.CombinedOutput()
	if err != nil {
		err = fmt.Errorf("上传失败：%s", strings.TrimSpace(string(out)))
	}
	s.audit(r, u, "volume.upload", vol, path.Join(rel, name), err)
	return done(w, err)
}
