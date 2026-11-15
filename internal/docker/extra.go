package docker

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ---------- events ----------

// Event is one entry of the daemon's event stream.
type Event struct {
	Type   string
	Action string
	Actor  struct {
		ID         string
		Attributes map[string]string
	}
	Time int64 `json:"time"`
}

// Events streams daemon events until ctx ends or the connection drops.
func (c *Client) Events(ctx context.Context, f map[string][]string, fn func(Event)) error {
	resp, err := c.request(ctx, c.stream, "GET", "/events", filters(f), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(bufio.NewReader(resp.Body))
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		fn(ev)
	}
}

// ---------- archive (file copy) ----------

// PathStat is the decoded X-Docker-Container-Path-Stat header.
type PathStat struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	Mode       uint32 `json:"mode"`
	Mtime      string `json:"mtime"`
	LinkTarget string `json:"linkTarget"`
}

// IsDir reports whether the Go os.FileMode bits mark a directory.
func (p PathStat) IsDir() bool { return p.Mode&(1<<31) != 0 }

// StatPath stats a path inside a container (running or not).
func (c *Client) StatPath(ctx context.Context, id, path string) (*PathStat, error) {
	resp, err := c.request(ctx, c.http, "HEAD", "/containers/"+url.PathEscape(id)+"/archive", url.Values{"path": {path}}, nil)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	raw, err := base64.StdEncoding.DecodeString(resp.Header.Get("X-Docker-Container-Path-Stat"))
	if err != nil {
		return nil, err
	}
	var st PathStat
	return &st, json.Unmarshal(raw, &st)
}

// GetArchive returns a tar stream of path inside the container.
func (c *Client) GetArchive(ctx context.Context, id, path string) (io.ReadCloser, error) {
	resp, err := c.request(ctx, c.stream, "GET", "/containers/"+url.PathEscape(id)+"/archive", url.Values{"path": {path}}, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// PutArchive extracts a tar stream into dir inside the container.
func (c *Client) PutArchive(ctx context.Context, id, dir string, tarStream io.Reader) error {
	u := c.base + "/containers/" + url.PathEscape(id) + "/archive?" + url.Values{"path": {dir}}.Encode()
	req, err := http.NewRequestWithContext(ctx, "PUT", u, tarStream)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var m struct{ Message string }
		if json.Unmarshal(data, &m) != nil || m.Message == "" {
			m.Message = strings.TrimSpace(string(data))
		}
		return &Error{Status: resp.StatusCode, Msg: m.Message}
	}
	return nil
}

// ---------- images ----------

type ImageInspect struct {
	Id          string
	RepoTags    []string
	RepoDigests []string
	Created     string
	Config      struct {
		ExposedPorts map[string]struct{}
	}
}

func (c *Client) InspectImage(ctx context.Context, ref string) (*ImageInspect, error) {
	var out ImageInspect
	return &out, c.do(ctx, "GET", "/images/"+ref+"/json", nil, nil, &out)
}

// RegistryAuth is sent as X-Registry-Auth.
type RegistryAuth struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	ServerAddress string `json:"serveraddress"`
}

// DistributionDigest asks the registry (through the daemon) for the current
// manifest digest of ref, without pulling it.
func (c *Client) DistributionDigest(ctx context.Context, ref string, auth *RegistryAuth) (string, error) {
	u := c.base + "/distribution/" + ref + "/json"
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	if auth != nil {
		b, _ := json.Marshal(auth)
		req.Header.Set("X-Registry-Auth", base64.URLEncoding.EncodeToString(b))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		var m struct{ Message string }
		if json.Unmarshal(data, &m) != nil || m.Message == "" {
			m.Message = strings.TrimSpace(string(data))
		}
		return "", &Error{Status: resp.StatusCode, Msg: m.Message}
	}
	var out struct {
		Descriptor struct {
			Digest string `json:"digest"`
		}
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	if out.Descriptor.Digest == "" {
		return "", fmt.Errorf("registry returned no digest")
	}
	return out.Descriptor.Digest, nil
}

// VolumeExists reports whether a named volume exists.
func (c *Client) VolumeExists(ctx context.Context, name string) (bool, error) {
	err := c.do(ctx, "GET", "/volumes/"+url.PathEscape(name), nil, nil, nil)
	if IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}
