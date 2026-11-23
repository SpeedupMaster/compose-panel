// Package compose shells out to the docker compose CLI.
package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Project describes how to invoke compose for one project.
type Project struct {
	Name      string   // compose project name (-p)
	Dir       string   // project directory
	Files     []string // compose files, relative to Dir or absolute
	EnvFile   string   // optional --env-file
	DockerCfg string   // optional DOCKER_CONFIG dir with registry credentials
}

func resolveComposeBin(lookPath func(string) (string, error), envBin string) (string, []string) {
	if envBin != "" {
		return envBin, nil
	}
	if p, err := lookPath("docker-compose"); err == nil && p != "" {
		return p, nil
	}
	const pluginPath = "/usr/libexec/docker/cli-plugins/docker-compose"
	if p, err := lookPath(pluginPath); err == nil && p != "" {
		return p, nil
	}
	return "docker", []string{"compose"}
}

func (p Project) baseArgs(extra ...string) []string {
	a := []string{"-p", p.Name, "--project-directory", p.Dir}
	for _, f := range p.Files {
		a = append(a, "-f", f)
	}
	if p.EnvFile != "" {
		a = append(a, "--env-file", p.EnvFile)
	}
	return append(a, extra...)
}

func (p Project) cmd(ctx context.Context, extra ...string) (*exec.Cmd, string) {
	bin, prefix := resolveComposeBin(exec.LookPath, strings.TrimSpace(os.Getenv("PANEL_COMPOSE_BIN")))
	args := append(prefix, p.baseArgs(extra...)...)
	c := exec.CommandContext(ctx, bin, args...)
	c.Dir = p.Dir
	c.Env = append(os.Environ(), "COMPOSE_ANSI=never", "COMPOSE_STATUS_STDOUT=1", "COMPOSE_PROGRESS=plain")
	if p.DockerCfg != "" {
		c.Env = append(c.Env, "DOCKER_CONFIG="+p.DockerCfg)
	}
	return c, bin + " " + strings.Join(args, " ")
}

// Run executes a compose subcommand, streaming combined output to w.
func (p Project) Run(ctx context.Context, w io.Writer, extra ...string) error {
	c, desc := p.cmd(ctx, extra...)
	c.Stdout, c.Stderr = w, w
	fmt.Fprintf(w, "$ %s\n", desc)
	return c.Run()
}

// Output runs a compose subcommand and returns stdout (stderr included in the error).
func (p Project) Output(ctx context.Context, stdin io.Reader, extra ...string) ([]byte, error) {
	c, _ := p.cmd(ctx, extra...)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr, c.Stdin = &out, &errb, stdin
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), fmt.Errorf("%s", msg)
	}
	return out.Bytes(), nil
}

// Config is the subset of `docker compose config --format json` the panel uses.
type Config struct {
	Name     string                    `json:"name"`
	Services map[string]ServiceConfig  `json:"services"`
	Volumes  map[string]map[string]any `json:"volumes"`
	Networks map[string]map[string]any `json:"networks"`
}

type ServiceConfig struct {
	Image       string           `json:"image"`
	Build       any              `json:"build"`
	Restart     string           `json:"restart"`
	MemLimit    any              `json:"mem_limit"`
	Ports       []map[string]any `json:"ports"`
	NetworkMode string           `json:"network_mode"`
	Networks    map[string]any   `json:"networks"`
	Healthcheck map[string]any   `json:"healthcheck"`
	Deploy      *struct {
		Resources struct {
			Limits map[string]any `json:"limits"`
		} `json:"resources"`
	} `json:"deploy"`
	Volumes []map[string]any `json:"volumes"`
	// Used to suggest a site's service, port and domain from the compose file.
	Labels      map[string]string  `json:"labels"`
	Environment map[string]*string `json:"environment"`
	Expose      []any              `json:"expose"`
}

func (s ServiceConfig) HasMemLimit() bool {
	if s.MemLimit != nil && fmt.Sprint(s.MemLimit) != "0" {
		return true
	}
	return s.Deploy != nil && s.Deploy.Resources.Limits["memory"] != nil
}

func (c *Config) ServiceNames() []string {
	names := make([]string, 0, len(c.Services))
	for n := range c.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// LoadConfig resolves and validates the project's compose files.
func (p Project) LoadConfig(ctx context.Context, stdin io.Reader) (*Config, error) {
	out, err := p.Output(ctx, stdin, "config", "--format", "json")
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(out, &c); err != nil {
		return nil, fmt.Errorf("parse compose config: %w", err)
	}
	return &c, nil
}

// Override builds the panel's compose override, attaching services to shared networks
// while keeping the networks each service already uses.
func Override(cfg *Config, extra map[string][]string) []byte {
	var b strings.Builder
	used := map[string]bool{}
	b.WriteString("# 由面板生成，请勿手动修改\n")
	// A bare "services:" parses as null, which compose rejects.
	hasSvc := false
	for _, name := range cfg.ServiceNames() {
		if len(extra[name]) > 0 && cfg.Services[name].NetworkMode == "" {
			hasSvc = true
		}
	}
	if hasSvc {
		b.WriteString("services:\n")
	} else {
		b.WriteString("services: {}\n")
	}
	for _, name := range cfg.ServiceNames() {
		nets := extra[name]
		if len(nets) == 0 {
			continue
		}
		svc := cfg.Services[name]
		if svc.NetworkMode != "" {
			continue
		}
		fmt.Fprintf(&b, "  %s:\n    networks:\n", quote(name))
		existing := make([]string, 0, len(svc.Networks))
		for n := range svc.Networks {
			existing = append(existing, n)
		}
		sort.Strings(existing)
		if len(existing) == 0 {
			existing = []string{"default"}
		}
		for _, n := range existing {
			fmt.Fprintf(&b, "      %s: {}\n", quote(n))
		}
		for _, n := range nets {
			if _, dup := svc.Networks[n]; dup {
				continue
			}
			fmt.Fprintf(&b, "      %s: {}\n", quote(n))
			used[n] = true
		}
	}
	if len(used) > 0 {
		b.WriteString("networks:\n")
		names := make([]string, 0, len(used))
		for n := range used {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Fprintf(&b, "  %s:\n    external: true\n    name: %s\n", quote(n), quote(n))
		}
	}
	return []byte(b.String())
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
