package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ExitError reports a non-zero exit status from a container exec or run invocation.
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	if msg := strings.TrimSpace(e.Stderr); msg != "" {
		return msg
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

// ExitCode extracts the process exit code when err is (or wraps) an *ExitError.
func ExitCode(err error) (int, bool) {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code, true
	}
	return 0, false
}

// ExecOptions configures a non-TTY container exec call over the Docker Engine API.
type ExecOptions struct {
	Cmd    []string
	Env    []string
	User   string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// RunOptions configures a one-shot or detached container created via the Docker Engine API.
type RunOptions struct {
	Name        string
	Image       string
	Entrypoint  []string
	Cmd         []string
	Env         []string
	Labels      map[string]string
	Binds       []string
	NetworkMode string // defaults to "none" when empty
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
}

type tailBuf struct {
	b   []byte
	max int
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append(t.b[:0], t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuf) String() string { return string(t.b) }

func closeWrite(conn net.Conn) {
	type halfCloser interface{ CloseWrite() error }
	if hc, ok := conn.(halfCloser); ok {
		_ = hc.CloseWrite()
	}
}

func (c *Client) hijack(ctx context.Context, method, path string, q url.Values, body []byte) (net.Conn, *bufio.Reader, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, c.network, c.addr)
	if err != nil {
		return nil, nil, err
	}
	target := path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n", method, target)
	if len(body) > 0 {
		fmt.Fprintf(&sb, "Content-Type: application/json\r\nContent-Length: %d\r\n\r\n", len(body))
	} else {
		sb.WriteString("Content-Length: 0\r\n\r\n")
	}
	if _, err := io.WriteString(conn, sb.String()); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if len(body) > 0 {
		if _, err := conn.Write(body); err != nil {
			conn.Close()
			return nil, nil, err
		}
	}
	br := bufio.NewReaderSize(conn, 32<<10)
	dummyReq, _ := http.NewRequest(method, c.base+target, nil)
	resp, err := http.ReadResponse(br, dummyReq)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		conn.Close()
		var m struct{ Message string }
		if json.Unmarshal(data, &m) != nil || m.Message == "" {
			m.Message = strings.TrimSpace(string(data))
		}
		return nil, nil, &Error{Status: resp.StatusCode, Msg: m.Message}
	}
	return conn, br, nil
}

func (c *Client) startExec(ctx context.Context, container string, opts ExecOptions) (string, net.Conn, *bufio.Reader, error) {
	var created struct{ Id string }
	body := map[string]any{
		"AttachStdin":  opts.Stdin != nil,
		"AttachStdout": true,
		"AttachStderr": true,
		"Tty":          false,
		"Cmd":          opts.Cmd,
	}
	if len(opts.Env) > 0 {
		body["Env"] = opts.Env
	}
	if opts.User != "" {
		body["User"] = opts.User
	}
	if err := c.do(ctx, "POST", "/containers/"+url.PathEscape(container)+"/exec", nil, body, &created); err != nil {
		return "", nil, nil, err
	}
	conn, br, err := c.hijack(ctx, "POST", "/exec/"+url.PathEscape(created.Id)+"/start", nil, []byte(`{"Detach":false,"Tty":false}`))
	if err != nil {
		return "", nil, nil, err
	}
	return created.Id, conn, br, nil
}

func (c *Client) execExitCode(execID string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var info struct {
		Running  bool
		ExitCode int
	}
	if err := c.do(ctx, "GET", "/exec/"+url.PathEscape(execID)+"/json", nil, nil, &info); err != nil {
		return 0, err
	}
	return info.ExitCode, nil
}

// Exec runs a non-TTY command inside container via the Docker Engine API.
func (c *Client) Exec(ctx context.Context, container string, opts ExecOptions) error {
	execID, conn, br, err := c.startExec(ctx, container, opts)
	if err != nil {
		return err
	}
	defer conn.Close()

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	if opts.Stdin != nil {
		go func() {
			_, _ = io.Copy(conn, opts.Stdin)
			closeWrite(conn)
		}()
	}

	tail := &tailBuf{max: 4000}
	var stderrW io.Writer = tail
	if opts.Stderr != nil {
		stderrW = io.MultiWriter(tail, opts.Stderr)
	}
	demuxErr := Demux(br, opts.Stdout, stderrW)
	_ = conn.Close()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	code, ierr := c.execExitCode(execID)
	if ierr == nil && code != 0 {
		return &ExitError{Code: code, Stderr: tail.String()}
	}
	if demuxErr != nil {
		return demuxErr
	}
	return ierr
}

type apiStream struct {
	pr      *io.PipeReader
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	waitErr error
}

func (s *apiStream) Read(p []byte) (int, error) { return s.pr.Read(p) }

func (s *apiStream) setWaitErr(err error) {
	s.mu.Lock()
	s.waitErr = err
	s.mu.Unlock()
}

func (s *apiStream) Close() error {
	s.once.Do(func() {
		s.cancel()
		_ = s.pr.Close()
	})
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waitErr
}

// ExecStdout runs a non-TTY command inside container and streams its stdout.
// A non-zero exit status is surfaced at EOF and on Close.
func (c *Client) ExecStdout(ctx context.Context, container string, opts ExecOptions) (io.ReadCloser, error) {
	sctx, cancel := context.WithCancel(ctx)
	execID, conn, br, err := c.startExec(sctx, container, opts)
	if err != nil {
		cancel()
		return nil, err
	}

	pr, pw := io.Pipe()
	st := &apiStream{pr: pr, cancel: cancel, done: make(chan struct{})}

	go func() {
		defer close(st.done)
		defer cancel()
		watchDone := make(chan struct{})
		go func() {
			select {
			case <-sctx.Done():
				_ = conn.Close()
			case <-watchDone:
			}
		}()
		defer close(watchDone)

		if opts.Stdin != nil {
			go func() {
				_, _ = io.Copy(conn, opts.Stdin)
				closeWrite(conn)
			}()
		}

		tail := &tailBuf{max: 4000}
		var stderrW io.Writer = tail
		if opts.Stderr != nil {
			stderrW = io.MultiWriter(tail, opts.Stderr)
		}
		demuxErr := Demux(br, pw, stderrW)
		_ = conn.Close()

		var finalErr error
		if sctx.Err() != nil {
			finalErr = sctx.Err()
		} else if code, ierr := c.execExitCode(execID); ierr == nil && code != 0 {
			finalErr = &ExitError{Code: code, Stderr: tail.String()}
		} else if demuxErr != nil {
			finalErr = demuxErr
		} else if ierr != nil {
			finalErr = ierr
		}
		st.setWaitErr(finalErr)
		_ = pw.CloseWithError(finalErr)
	}()

	return st, nil
}

// PullImage asks the Docker daemon to pull ref (e.g. "alpine:3.20").
func (c *Client) PullImage(ctx context.Context, ref string) error {
	q := url.Values{"fromImage": {ref}}
	lastSlash := strings.LastIndex(ref, "/")
	if !strings.Contains(ref, "@") && strings.LastIndex(ref, ":") <= lastSlash {
		q.Set("tag", "latest")
	}
	resp, err := c.request(ctx, c.stream, "POST", "/images/create", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if msg.Error != "" {
			return fmt.Errorf("pull %s: %s", ref, msg.Error)
		}
		if msg.ErrorDetail.Message != "" {
			return fmt.Errorf("pull %s: %s", ref, msg.ErrorDetail.Message)
		}
	}
}

func (c *Client) createContainer(ctx context.Context, opts RunOptions, detached bool) (string, error) {
	netMode := opts.NetworkMode
	if netMode == "" {
		netMode = "none"
	}
	hostCfg := map[string]any{"NetworkMode": netMode}
	if len(opts.Binds) > 0 {
		hostCfg["Binds"] = opts.Binds
	}
	body := map[string]any{
		"Image":        opts.Image,
		"Tty":          false,
		"OpenStdin":    opts.Stdin != nil,
		"StdinOnce":    opts.Stdin != nil,
		"AttachStdin":  !detached && opts.Stdin != nil,
		"AttachStdout": !detached,
		"AttachStderr": !detached,
		"HostConfig":   hostCfg,
	}
	if opts.Entrypoint != nil {
		body["Entrypoint"] = opts.Entrypoint
	}
	if opts.Cmd != nil {
		body["Cmd"] = opts.Cmd
	}
	if len(opts.Env) > 0 {
		body["Env"] = opts.Env
	}
	if len(opts.Labels) > 0 {
		body["Labels"] = opts.Labels
	}
	q := url.Values{}
	if opts.Name != "" {
		q.Set("name", opts.Name)
	}
	var created struct{ Id string }
	err := c.do(ctx, "POST", "/containers/create", q, body, &created)
	if IsNotFound(err) {
		if perr := c.PullImage(ctx, opts.Image); perr != nil {
			return "", perr
		}
		err = c.do(ctx, "POST", "/containers/create", q, body, &created)
	}
	if err != nil {
		return "", err
	}
	return created.Id, nil
}

func (c *Client) waitContainer(ctx context.Context, id string) (int, error) {
	var res struct {
		StatusCode int
		Error      *struct{ Message string }
	}
	resp, err := c.request(ctx, c.stream, "POST", "/containers/"+url.PathEscape(id)+"/wait", nil, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return 0, err
	}
	if res.Error != nil && res.Error.Message != "" {
		return res.StatusCode, errors.New(res.Error.Message)
	}
	return res.StatusCode, nil
}

// StartDetached creates (auto-pulling if missing) and starts a container in detached mode.
func (c *Client) StartDetached(ctx context.Context, opts RunOptions) (string, error) {
	id, err := c.createContainer(ctx, opts, true)
	if err != nil {
		return "", err
	}
	if err := c.do(ctx, "POST", "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil); err != nil {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = c.RemoveContainerWithVolumes(rctx, id, true, true)
		cancel()
		return "", err
	}
	return id, nil
}

func (c *Client) attachAndStart(ctx context.Context, id string, withStdin bool) (net.Conn, *bufio.Reader, error) {
	q := url.Values{"stream": {"1"}, "stdout": {"1"}, "stderr": {"1"}}
	if withStdin {
		q.Set("stdin", "1")
	}
	conn, br, err := c.hijack(ctx, "POST", "/containers/"+url.PathEscape(id)+"/attach", q, nil)
	if err != nil {
		return nil, nil, err
	}
	if err := c.do(ctx, "POST", "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, br, nil
}

// RunContainer runs a throwaway container (like `docker run --rm`), streaming stdin/stdout/stderr
// and removing the container and anonymous volumes before returning.
func (c *Client) RunContainer(ctx context.Context, opts RunOptions) error {
	id, err := c.createContainer(ctx, opts, false)
	if err != nil {
		return err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = c.RemoveContainerWithVolumes(rctx, id, true, true)
		cancel()
	}()

	conn, br, err := c.attachAndStart(ctx, id, opts.Stdin != nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	if opts.Stdin != nil {
		go func() {
			_, _ = io.Copy(conn, opts.Stdin)
			closeWrite(conn)
		}()
	}

	tail := &tailBuf{max: 4000}
	var stderrW io.Writer = tail
	if opts.Stderr != nil {
		stderrW = io.MultiWriter(tail, opts.Stderr)
	}
	demuxErr := Demux(br, opts.Stdout, stderrW)
	_ = conn.Close()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	wctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	code, werr := c.waitContainer(wctx, id)
	cancel()
	if werr == nil && code != 0 {
		return &ExitError{Code: code, Stderr: tail.String()}
	}
	if demuxErr != nil {
		return demuxErr
	}
	return werr
}

// RunContainerStdout runs a throwaway container and streams its stdout.
// Closing the reader (or reaching EOF) waits for exit and removes the container.
func (c *Client) RunContainerStdout(ctx context.Context, opts RunOptions) (io.ReadCloser, error) {
	sctx, cancel := context.WithCancel(ctx)
	id, err := c.createContainer(sctx, opts, false)
	if err != nil {
		cancel()
		return nil, err
	}
	conn, br, err := c.attachAndStart(sctx, id, opts.Stdin != nil)
	if err != nil {
		rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = c.RemoveContainerWithVolumes(rctx, id, true, true)
		rcancel()
		cancel()
		return nil, err
	}

	pr, pw := io.Pipe()
	st := &apiStream{pr: pr, cancel: cancel, done: make(chan struct{})}

	go func() {
		defer close(st.done)
		defer cancel()
		defer func() {
			rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = c.RemoveContainerWithVolumes(rctx, id, true, true)
			rcancel()
		}()
		watchDone := make(chan struct{})
		go func() {
			select {
			case <-sctx.Done():
				_ = conn.Close()
			case <-watchDone:
			}
		}()
		defer close(watchDone)

		if opts.Stdin != nil {
			go func() {
				_, _ = io.Copy(conn, opts.Stdin)
				closeWrite(conn)
			}()
		}

		tail := &tailBuf{max: 4000}
		var stderrW io.Writer = tail
		if opts.Stderr != nil {
			stderrW = io.MultiWriter(tail, opts.Stderr)
		}
		demuxErr := Demux(br, pw, stderrW)
		_ = conn.Close()

		var finalErr error
		if sctx.Err() != nil {
			finalErr = sctx.Err()
		} else {
			wctx, wcancel := context.WithTimeout(context.Background(), 30*time.Second)
			code, werr := c.waitContainer(wctx, id)
			wcancel()
			if werr == nil && code != 0 {
				finalErr = &ExitError{Code: code, Stderr: tail.String()}
			} else if demuxErr != nil {
				finalErr = demuxErr
			} else if werr != nil {
				finalErr = werr
			}
		}
		st.setWaitErr(finalErr)
		_ = pw.CloseWithError(finalErr)
	}()

	return st, nil
}
