// Package docker is a small Docker Engine API client over the unix socket.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	http    *http.Client
	stream  *http.Client
	network string
	addr    string
	base    string
}

// New accepts unix:///path or tcp://host:port.
func New(host string) *Client {
	c := &Client{base: "http://docker"}
	switch {
	case strings.HasPrefix(host, "unix://"):
		c.network, c.addr = "unix", strings.TrimPrefix(host, "unix://")
	case strings.HasPrefix(host, "tcp://"):
		c.network, c.addr = "tcp", strings.TrimPrefix(host, "tcp://")
	default:
		c.network, c.addr = "unix", host
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, c.network, c.addr)
	}
	c.http = &http.Client{
		Transport: &http.Transport{
			DialContext:         dial,
			MaxIdleConns:        16,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     30 * time.Second,
		},
		Timeout: 60 * time.Second,
	}
	c.stream = &http.Client{
		Transport: &http.Transport{
			DialContext:         dial,
			MaxIdleConns:        8,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     30 * time.Second,
		},
	}
	return c
}

type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return fmt.Sprintf("docker: %s (%d)", e.Msg, e.Status) }

func IsNotFound(err error) bool {
	e, ok := err.(*Error)
	return ok && e.Status == 404
}

func (c *Client) request(ctx context.Context, cl *http.Client, method, path string, q url.Values, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var m struct{ Message string }
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(data, &m) != nil || m.Message == "" {
			m.Message = strings.TrimSpace(string(data))
		}
		return nil, &Error{Status: resp.StatusCode, Msg: m.Message}
	}
	return resp, nil
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	resp, err := c.request(ctx, c.http, method, path, q, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func filters(f map[string][]string) url.Values {
	q := url.Values{}
	if len(f) > 0 {
		b, _ := json.Marshal(f)
		q.Set("filters", string(b))
	}
	return q
}

// ---------- system ----------

func (c *Client) Ping(ctx context.Context) error { return c.do(ctx, "GET", "/_ping", nil, nil, nil) }

func (c *Client) Info(ctx context.Context) (map[string]any, error) {
	var m map[string]any
	return m, c.do(ctx, "GET", "/info", nil, nil, &m)
}

func (c *Client) Version(ctx context.Context) (map[string]any, error) {
	var m map[string]any
	return m, c.do(ctx, "GET", "/version", nil, nil, &m)
}

type DiskUsage struct {
	LayersSize int64
	Images     []Image
	Containers []struct {
		Id         string
		SizeRw     int64
		State      string
		Names      []string
		SizeRootFs int64
	}
	Volumes []struct {
		Name      string
		UsageData struct{ Size, RefCount int64 }
	}
	BuildCache []struct {
		Size   int64
		InUse  bool
		Shared bool
	}
}

func (c *Client) DiskUsage(ctx context.Context) (*DiskUsage, error) {
	var d DiskUsage
	return &d, c.do(ctx, "GET", "/system/df", nil, nil, &d)
}

// ---------- containers ----------

type Port struct {
	IP          string
	PrivatePort int
	PublicPort  int
	Type        string
}

type Mount struct {
	Type        string
	Name        string
	Source      string
	Destination string
	RW          bool
}

type Container struct {
	Id      string
	Names   []string
	Image   string
	ImageID string
	Command string
	Created int64
	State   string
	Status  string
	Ports   []Port
	Labels  map[string]string
	Mounts  []Mount
}

func (c Container) Name() string {
	if len(c.Names) == 0 {
		return c.Id[:12]
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

func (c *Client) ListContainers(ctx context.Context, all bool, f map[string][]string) ([]Container, error) {
	q := filters(f)
	if all {
		q.Set("all", "1")
	}
	var out []Container
	return out, c.do(ctx, "GET", "/containers/json", q, nil, &out)
}

// ProjectContainers lists containers of a compose project.
func (c *Client) ProjectContainers(ctx context.Context, project string) ([]Container, error) {
	return c.ListContainers(ctx, true, map[string][]string{"label": {"com.docker.compose.project=" + project}})
}

type ContainerJSON struct {
	Id           string
	Name         string
	Created      string
	RestartCount int
	State        struct {
		Status     string
		Running    bool
		Restarting bool
		OOMKilled  bool
		ExitCode   int
		StartedAt  string
		FinishedAt string
		Health     *struct {
			Status        string
			FailingStreak int
		}
	}
	Config struct {
		Image       string
		Tty         bool
		Cmd         []string
		Entrypoint  []string
		Env         []string
		Labels      map[string]string
		Healthcheck *struct {
			Test []string
		}
	}
	HostConfig struct {
		Memory        int64
		NanoCpus      int64
		RestartPolicy struct{ Name string }
	}
	Mounts []Mount
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string
			Aliases   []string
		}
	}
}

func (c *Client) InspectContainer(ctx context.Context, id string) (*ContainerJSON, error) {
	var out ContainerJSON
	return &out, c.do(ctx, "GET", "/containers/"+url.PathEscape(id)+"/json", nil, nil, &out)
}

func (c *Client) ContainerAction(ctx context.Context, id, action string) error {
	switch action {
	case "start", "stop", "restart", "kill", "pause", "unpause":
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	q := url.Values{}
	if action == "stop" || action == "restart" {
		q.Set("t", "15")
	}
	return c.do(ctx, "POST", "/containers/"+url.PathEscape(id)+"/"+action, q, nil, nil)
}

func (c *Client) RemoveContainer(ctx context.Context, id string, force bool) error {
	return c.RemoveContainerWithVolumes(ctx, id, force, false)
}

func (c *Client) RemoveContainerWithVolumes(ctx context.Context, id string, force, removeVolumes bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	if removeVolumes {
		q.Set("v", "1")
	}
	return c.do(ctx, "DELETE", "/containers/"+url.PathEscape(id), q, nil, nil)
}

type Stats struct {
	CPUPercent float64 `json:"cpuPercent"`
	MemUsage   int64   `json:"memUsage"`
	MemLimit   int64   `json:"memLimit"`
	NetRx      int64   `json:"netRx"`
	NetTx      int64   `json:"netTx"`
	BlockRead  int64   `json:"blockRead"`
	BlockWrite int64   `json:"blockWrite"`
	PIDs       int64   `json:"pids"`
}

type rawStats struct {
	CPUStats    cpuStats `json:"cpu_stats"`
	PreCPUStats cpuStats `json:"precpu_stats"`
	MemoryStats struct {
		Usage int64            `json:"usage"`
		Limit int64            `json:"limit"`
		Stats map[string]int64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes int64 `json:"rx_bytes"`
		TxBytes int64 `json:"tx_bytes"`
	} `json:"networks"`
	BlkioStats struct {
		IoServiceBytesRecursive []struct {
			Op    string `json:"op"`
			Value int64  `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
	PidsStats struct {
		Current int64 `json:"current"`
	} `json:"pids_stats"`
}

type cpuStats struct {
	CPUUsage struct {
		TotalUsage int64 `json:"total_usage"`
	} `json:"cpu_usage"`
	SystemUsage int64 `json:"system_cpu_usage"`
	OnlineCPUs  int64 `json:"online_cpus"`
}

// ContainerStats takes one sample (the daemon waits ~1s for the CPU delta).
func (c *Client) ContainerStats(ctx context.Context, id string) (*Stats, error) {
	var r rawStats
	if err := c.do(ctx, "GET", "/containers/"+url.PathEscape(id)+"/stats", url.Values{"stream": {"0"}}, nil, &r); err != nil {
		return nil, err
	}
	s := &Stats{MemLimit: r.MemoryStats.Limit, PIDs: r.PidsStats.Current}
	cache := r.MemoryStats.Stats["inactive_file"]
	if cache == 0 {
		cache = r.MemoryStats.Stats["cache"]
	}
	s.MemUsage = r.MemoryStats.Usage - cache
	if s.MemUsage < 0 {
		s.MemUsage = r.MemoryStats.Usage
	}
	cpuDelta := float64(r.CPUStats.CPUUsage.TotalUsage - r.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(r.CPUStats.SystemUsage - r.PreCPUStats.SystemUsage)
	cpus := float64(r.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = 1
	}
	if sysDelta > 0 && cpuDelta > 0 {
		s.CPUPercent = cpuDelta / sysDelta * cpus * 100
	}
	for _, n := range r.Networks {
		s.NetRx += n.RxBytes
		s.NetTx += n.TxBytes
	}
	for _, b := range r.BlkioStats.IoServiceBytesRecursive {
		switch strings.ToLower(b.Op) {
		case "read":
			s.BlockRead += b.Value
		case "write":
			s.BlockWrite += b.Value
		}
	}
	return s, nil
}

// Logs returns a reader of plain log lines (demultiplexed when the container has no TTY).
func (c *Client) Logs(ctx context.Context, id string, tail string, follow, timestamps bool, since int64) (io.ReadCloser, error) {
	info, err := c.InspectContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {tail}}
	if follow {
		q.Set("follow", "1")
	}
	if timestamps {
		q.Set("timestamps", "1")
	}
	if since > 0 {
		q.Set("since", fmt.Sprint(since))
	}
	resp, err := c.request(ctx, c.stream, "GET", "/containers/"+url.PathEscape(id)+"/logs", q, nil)
	if err != nil {
		return nil, err
	}
	if info.Config.Tty {
		return resp.Body, nil
	}
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(Demux(resp.Body, pw, pw))
		resp.Body.Close()
	}()
	return pr, nil
}

// Demux splits Docker's multiplexed stdout/stderr stream.
func Demux(r io.Reader, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 32<<10)
	}
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		n := int64(binary.BigEndian.Uint32(hdr[4:]))
		w := stdout
		if hdr[0] == 2 {
			w = stderr
		}
		if _, err := io.CopyN(w, br, n); err != nil {
			return err
		}
	}
}

// ---------- exec (interactive) ----------

type HijackedConn struct {
	net.Conn
	r *bufio.Reader
}

func (h *HijackedConn) Read(p []byte) (int, error) { return h.r.Read(p) }

// ExecTTY creates an interactive exec session and returns the raw stream.
func (c *Client) ExecTTY(ctx context.Context, id string, cmd []string, user string) (*HijackedConn, string, error) {
	var created struct{ Id string }
	body := map[string]any{
		"AttachStdin": true, "AttachStdout": true, "AttachStderr": true,
		"Tty": true, "Cmd": cmd, "User": user,
		"Env": []string{"TERM=xterm-256color"},
	}
	if err := c.do(ctx, "POST", "/containers/"+url.PathEscape(id)+"/exec", nil, body, &created); err != nil {
		return nil, "", err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, c.network, c.addr)
	if err != nil {
		return nil, "", err
	}
	payload := `{"Detach":false,"Tty":true}`
	fmt.Fprintf(conn, "POST /exec/%s/start HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: %d\r\n\r\n%s",
		created.Id, len(payload), payload)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, "", err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, "", fmt.Errorf("exec start: %s", resp.Status)
	}
	return &HijackedConn{Conn: conn, r: br}, created.Id, nil
}

func (c *Client) ExecResize(ctx context.Context, execID string, rows, cols int) error {
	q := url.Values{"h": {fmt.Sprint(rows)}, "w": {fmt.Sprint(cols)}}
	return c.do(ctx, "POST", "/exec/"+execID+"/resize", q, nil, nil)
}

// ---------- images / volumes / networks ----------

type Image struct {
	Id          string
	RepoTags    []string
	RepoDigests []string
	Created     int64
	Size        int64
	Containers  int64
}

func (c *Client) ListImages(ctx context.Context) ([]Image, error) {
	var out []Image
	return out, c.do(ctx, "GET", "/images/json", nil, nil, &out)
}

func (c *Client) RemoveImage(ctx context.Context, id string, force bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	return c.do(ctx, "DELETE", "/images/"+url.PathEscape(id), q, nil, nil)
}

type Volume struct {
	Name       string
	Driver     string
	Mountpoint string
	CreatedAt  string
	Labels     map[string]string
}

func (c *Client) ListVolumes(ctx context.Context) ([]Volume, error) {
	var out struct{ Volumes []Volume }
	return out.Volumes, c.do(ctx, "GET", "/volumes", nil, nil, &out)
}

func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	return c.do(ctx, "DELETE", "/volumes/"+url.PathEscape(name), nil, nil, nil)
}

func (c *Client) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	return c.do(ctx, "POST", "/volumes/create", nil, map[string]any{"Name": name, "Labels": labels}, nil)
}

type Network struct {
	Id         string
	Name       string
	Driver     string
	Scope      string
	Internal   bool
	Labels     map[string]string
	IPAM       struct{ Config []struct{ Subnet string } }
	Containers map[string]struct{ Name string }
}

func (c *Client) ListNetworks(ctx context.Context) ([]Network, error) {
	var out []Network
	return out, c.do(ctx, "GET", "/networks", nil, nil, &out)
}

func (c *Client) InspectNetwork(ctx context.Context, id string) (*Network, error) {
	var n Network
	return &n, c.do(ctx, "GET", "/networks/"+url.PathEscape(id), nil, nil, &n)
}

func (c *Client) RemoveNetwork(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/networks/"+url.PathEscape(id), nil, nil, nil)
}

// EnsureNetwork creates a bridge network if it does not exist.
func (c *Client) EnsureNetwork(ctx context.Context, name string) error {
	if _, err := c.InspectNetwork(ctx, name); err == nil {
		return nil
	} else if !IsNotFound(err) {
		return err
	}
	return c.do(ctx, "POST", "/networks/create", nil, map[string]any{
		"Name": name, "Driver": "bridge", "Labels": map[string]string{"panel.managed": "true"},
	}, nil)
}

// ConnectNetwork attaches a container to a network, ignoring "already connected".
func (c *Client) ConnectNetwork(ctx context.Context, network, container string) error {
	err := c.do(ctx, "POST", "/networks/"+url.PathEscape(network)+"/connect", nil, map[string]any{"Container": container}, nil)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

type PruneReport struct {
	SpaceReclaimed int64 `json:"spaceReclaimed"`
	Deleted        int   `json:"deleted"`
}

// Prune runs one of the prune endpoints: images, containers, volumes, build.
func (c *Client) Prune(ctx context.Context, kind string, f map[string][]string) (PruneReport, error) {
	path := map[string]string{"images": "/images/prune", "containers": "/containers/prune", "volumes": "/volumes/prune", "build": "/build/prune"}[kind]
	if path == "" {
		return PruneReport{}, fmt.Errorf("unknown prune kind %q", kind)
	}
	var raw struct {
		SpaceReclaimed    int64
		ImagesDeleted     []any
		ContainersDeleted []string
		VolumesDeleted    []string
		CachesDeleted     []string
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	resp, err := c.request(cctx, c.stream, "POST", path, filters(f), nil)
	if err != nil {
		return PruneReport{}, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return PruneReport{}, err
	}
	return PruneReport{
		SpaceReclaimed: raw.SpaceReclaimed,
		Deleted:        len(raw.ImagesDeleted) + len(raw.ContainersDeleted) + len(raw.VolumesDeleted) + len(raw.CachesDeleted),
	}, nil
}
