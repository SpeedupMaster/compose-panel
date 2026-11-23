package compose

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestOverrideSkipsNetworkModeServices(t *testing.T) {
	raw := `{
		"name": "wechat-telegram-bridge",
		"services": {
			"token-init": {"image": "busybox:1.37", "network_mode": "none"},
			"agent-wechat": {"image": "ghcr.io/thisnick/agent-wechat-native:latest", "network_mode": "host"},
			"telegram-bridge": {"image": "ghcr.io/speedupmaster/wechat-telegram-bridge:latest", "network_mode": "host"}
		}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if cfg.Services["token-init"].NetworkMode != "none" {
		t.Fatalf("token-init network_mode = %q, want %q", cfg.Services["token-init"].NetworkMode, "none")
	}

	got := string(Override(&cfg, map[string][]string{
		"token-init":      {"panel-proxy", "panel-db"},
		"agent-wechat":    {"panel-proxy", "panel-db"},
		"telegram-bridge": {"panel-proxy", "panel-db"},
	}))
	if !strings.Contains(got, "services: {}\n") {
		t.Fatalf("Override() = %q, want empty services map", got)
	}
	if strings.Contains(got, "networks:") {
		t.Fatalf("Override() unexpectedly emitted networks:\n%s", got)
	}
}

func TestOverrideMixedNetworkModeAndBridgeServices(t *testing.T) {
	cfg := &Config{
		Services: map[string]ServiceConfig{
			"init": {NetworkMode: "none"},
			"web":  {Networks: map[string]any{"internal": map[string]any{}}},
		},
	}
	got := string(Override(cfg, map[string][]string{
		"init": {"panel-proxy"},
		"web":  {"panel-proxy"},
	}))
	if strings.Contains(got, `"init":`) {
		t.Fatalf("Override() should not include network_mode service init:\n%s", got)
	}
	if !strings.Contains(got, `"web":`) || !strings.Contains(got, `"internal": {}`) || !strings.Contains(got, `"panel-proxy": {}`) {
		t.Fatalf("Override() missing expected web networks:\n%s", got)
	}
}

func TestResolveComposeBinPrefersStandaloneAndFallsBack(t *testing.T) {
	notFound := func(string) (string, error) { return "", errors.New("not found") }

	// 1. Explicit env override.
	bin, prefix := resolveComposeBin(notFound, "/custom/docker-compose")
	if bin != "/custom/docker-compose" || len(prefix) != 0 {
		t.Errorf("env override = (%q, %v), want (/custom/docker-compose, [])", bin, prefix)
	}

	// 2. Standalone docker-compose in PATH.
	bin, prefix = resolveComposeBin(func(name string) (string, error) {
		if name == "docker-compose" {
			return "/usr/local/bin/docker-compose", nil
		}
		return "", errors.New("not found")
	}, "")
	if bin != "/usr/local/bin/docker-compose" || len(prefix) != 0 {
		t.Errorf("standalone = (%q, %v), want (/usr/local/bin/docker-compose, [])", bin, prefix)
	}

	// 3. Fallback to docker compose plugin when standalone is missing.
	bin, prefix = resolveComposeBin(notFound, "")
	if bin != "docker" || !reflect.DeepEqual(prefix, []string{"compose"}) {
		t.Errorf("fallback = (%q, %v), want (docker, [compose])", bin, prefix)
	}
}
