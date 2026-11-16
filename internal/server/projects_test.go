package server

import (
	"compose-panel/internal/compose"
	"compose-panel/internal/config"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func boolPtr(v bool) *bool { return &v }

func TestComposeWaitArgs(t *testing.T) {
	tests := []struct {
		name string
		opts ProjectOptions
		want []string
	}{
		{
			name: "default waits 180s",
			opts: ProjectOptions{},
			want: []string{"--wait", "--wait-timeout", "180"},
		},
		{
			name: "explicit custom wait timeout",
			opts: ProjectOptions{Wait: boolPtr(true), WaitTimeout: 45},
			want: []string{"--wait", "--wait-timeout", "45"},
		},
		{
			name: "wait disabled omits flags",
			opts: ProjectOptions{Wait: boolPtr(false), WaitTimeout: 45},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := composeWaitArgs(tt.opts)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("composeWaitArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildProjectAction(t *testing.T) {
	app := &Project{ID: 1, Name: "e2e-web", Kind: "app"}
	db := &Project{ID: 2, Name: "e2e-pg", Kind: "database"}

	tests := []struct {
		name    string
		project *Project
		action  string
		service string
		want    projectActionPlan
		wantErr int
	}{
		{
			name:    "recreate whole app project defaults to wait 180 and remove-orphans",
			project: app,
			action:  "recreate",
			want: projectActionPlan{
				Title:     "重建容器 e2e-web",
				SyncCaddy: true,
				Steps: []projectActionStep{
					{
						Log:           "==> 强制拉取镜像",
						Args:          []string{"pull", "--ignore-buildable"},
						ProgressAfter: 50,
					},
					{
						Log:           "==> 重建并启动容器",
						Args:          []string{"up", "-d", "--force-recreate", "--remove-orphans", "--wait", "--wait-timeout", "180"},
						ProgressAfter: 100,
						DumpUnhealthy: true,
					},
				},
			},
		},
		{
			name:    "recreate single service uses no-deps and omits remove-orphans",
			project: &Project{ID: 3, Name: "demo", Kind: "app", Options: ProjectOptions{Wait: boolPtr(true), WaitTimeout: 60}},
			action:  "recreate",
			service: " web ",
			want: projectActionPlan{
				Title:     "重建容器 demo/web",
				SyncCaddy: true,
				Steps: []projectActionStep{
					{
						Log:           "==> 强制拉取镜像",
						Args:          []string{"pull", "--ignore-buildable", "web"},
						ProgressAfter: 50,
					},
					{
						Log:           "==> 重建并启动容器",
						Args:          []string{"up", "-d", "--force-recreate", "--no-deps", "--wait", "--wait-timeout", "60", "web"},
						ProgressAfter: 100,
						DumpUnhealthy: true,
					},
				},
			},
		},
		{
			name:    "recreate respects wait disabled",
			project: &Project{ID: 4, Name: "nowait", Kind: "app", Options: ProjectOptions{Wait: boolPtr(false)}},
			action:  "recreate",
			service: "worker",
			want: projectActionPlan{
				Title:     "重建容器 nowait/worker",
				SyncCaddy: true,
				Steps: []projectActionStep{
					{
						Log:           "==> 强制拉取镜像",
						Args:          []string{"pull", "--ignore-buildable", "worker"},
						ProgressAfter: 50,
					},
					{
						Log:           "==> 重建并启动容器",
						Args:          []string{"up", "-d", "--force-recreate", "--no-deps", "worker"},
						ProgressAfter: 100,
						DumpUnhealthy: true,
					},
				},
			},
		},
		{
			name:    "recreate rejects database project",
			project: db,
			action:  "recreate",
			wantErr: 400,
		},
		{
			name:    "recreate rejects database project even with service",
			project: db,
			action:  "recreate",
			service: "db",
			wantErr: 400,
		},
		{
			name:    "restart service preserves single-step behavior",
			project: app,
			action:  "restart",
			service: "web",
			want: projectActionPlan{
				Title: "重启 e2e-web/web",
				Steps: []projectActionStep{
					{Args: []string{"restart", "web"}},
				},
			},
		},
		{
			name:    "down whole project",
			project: app,
			action:  "down",
			want: projectActionPlan{
				Title: "下线 e2e-web",
				Steps: []projectActionStep{
					{Args: []string{"down", "--remove-orphans"}},
				},
			},
		},
		{
			name:    "down rejects single service",
			project: app,
			action:  "down",
			service: "web",
			wantErr: 400,
		},
		{
			name:    "unknown action returns 400",
			project: app,
			action:  "destroy-universe",
			wantErr: 400,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildProjectAction(tt.project, tt.action, tt.service)
			if tt.wantErr != 0 {
				var ae *apiError
				if !errors.As(err, &ae) || ae.Status != tt.wantErr {
					t.Fatalf("buildProjectAction() err = %v, want *apiError status %d", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildProjectAction() unexpected err: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("buildProjectAction() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestHandleProjectActionValidation(t *testing.T) {
	tmp := t.TempDir()
	srv, err := New(config.Config{
		DataDir:     tmp,
		ProjectsDir: tmp,
		BackupsDir:  tmp,
		DockerHost:  "unix:///nonexistent.sock",
	}, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { _ = srv.DB().Close() })

	now := time.Now().Unix()
	dbRes, err := srv.DB().Exec(`INSERT INTO projects(name, kind, created_at, updated_at) VALUES('db-1', 'database', ?, ?)`, now, now)
	if err != nil {
		t.Fatalf("insert db project: %v", err)
	}
	dbID, _ := dbRes.LastInsertId()
	appRes, err := srv.DB().Exec(`INSERT INTO projects(name, kind, created_at, updated_at) VALUES('app-1', 'app', ?, ?)`, now, now)
	if err != nil {
		t.Fatalf("insert app project: %v", err)
	}
	appID, _ := appRes.LastInsertId()
	admin := &User{ID: 1, Username: "admin", Role: "admin", Level: roleAdmin}

	t.Run("recreate database project returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/projects/action", strings.NewReader(`{"action":"recreate"}`))
		req.SetPathValue("id", fmt.Sprint(dbID))
		w := httptest.NewRecorder()
		err := srv.handleProjectAction(w, req, admin)
		var ae *apiError
		if !errors.As(err, &ae) || ae.Status != 400 {
			t.Fatalf("handleProjectAction() err = %v, want 400 *apiError", err)
		}
	})

	t.Run("down single service returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/projects/action", strings.NewReader(`{"action":"down","service":"web"}`))
		req.SetPathValue("id", fmt.Sprint(appID))
		w := httptest.NewRecorder()
		err := srv.handleProjectAction(w, req, admin)
		var ae *apiError
		if !errors.As(err, &ae) || ae.Status != 400 {
			t.Fatalf("handleProjectAction() err = %v, want 400 *apiError", err)
		}
	})

	t.Run("unknown action returns 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/projects/action", strings.NewReader(`{"action":"invalid"}`))
		req.SetPathValue("id", fmt.Sprint(appID))
		w := httptest.NewRecorder()
		err := srv.handleProjectAction(w, req, admin)
		var ae *apiError
		if !errors.As(err, &ae) || ae.Status != 400 {
			t.Fatalf("handleProjectAction() err = %v, want 400 *apiError", err)
		}
	})
}

func TestComposeExtraNetworks(t *testing.T) {
	cfg := &compose.Config{
		Services: map[string]compose.ServiceConfig{
			"init":       {NetworkMode: "none"},
			"hostmode":   {NetworkMode: "host"},
			"shared":     {NetworkMode: "service:web"},
			"hostnet":    {Networks: map[string]any{"host": map[string]any{}}},
			"web":        {},
			"background": {Networks: map[string]any{"internal": map[string]any{}}},
		},
	}

	got := composeExtraNetworks(&Project{Kind: "app"}, cfg, "panel-proxy", "panel-db")
	want := map[string][]string{
		"web":        {"panel-proxy", "panel-db"},
		"background": {"panel-proxy", "panel-db"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("composeExtraNetworks() = %#v, want %#v", got, want)
	}

	if got := composeExtraNetworks(&Project{Kind: "app", Options: ProjectOptions{SkipNetworks: true}}, cfg, "panel-proxy", "panel-db"); len(got) != 0 {
		t.Fatalf("SkipNetworks=true should return empty map, got %#v", got)
	}
	if got := composeExtraNetworks(&Project{Kind: "database"}, cfg, "panel-proxy", "panel-db"); len(got) != 0 {
		t.Fatalf("database project should return empty map, got %#v", got)
	}
}
