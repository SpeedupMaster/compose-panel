package server

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

type backupDirOption struct {
	Path  string `json:"path"`
	Label string `json:"label,omitempty"`
	Hint  string `json:"hint,omitempty"`
}

var (
	composeVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

	ignoredScanDirs = map[string]bool{
		"node_modules": true,
		"__pycache__":  true,
		"vendor":       true,
		"dist":         true,
		"build":        true,
	}

	likelyFileExts = map[string]bool{
		".conf": true, ".cnf": true, ".cfg": true, ".ini": true, ".env": true,
		".yml": true, ".yaml": true, ".json": true, ".toml": true, ".xml": true,
		".crt": true, ".cer": true, ".key": true, ".pem": true, ".pub": true,
		".sh": true, ".sql": true, ".txt": true, ".md": true, ".log": true,
		".html": true, ".htm": true, ".css": true, ".js": true, ".mjs": true,
		".sock": true, ".pid": true, ".lock": true,
	}
)

// detectProjectDirs discovers backupable subdirectories inside a project's working directory
// by combining Compose bind-mount paths (with common parent grouping) and existing disk folders.
func (s *Server) detectProjectDirs(p *Project) []backupDirOption {
	projDir := filepath.Clean(s.projectDir(p))
	envMap := make(map[string]string, len(p.Env))
	for _, e := range p.Env {
		if e.Key != "" {
			envMap[e.Key] = e.Value
		}
	}

	mountTargets := map[string][]string{}
	addMount := func(rawSource, target, baseDir string) {
		src := strings.TrimSpace(expandComposeEnv(rawSource, envMap))
		if src == "" || strings.Contains(src, "$") || !isBindSource(src) {
			return
		}
		var abs string
		if filepath.IsAbs(src) {
			abs = filepath.Clean(src)
		} else {
			abs = filepath.Join(baseDir, filepath.FromSlash(src))
		}
		rel, err := filepath.Rel(projDir, abs)
		if err != nil {
			return
		}
		rel = strings.Trim(filepath.ToSlash(rel), "/")
		if rel == "" || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
			return
		}
		if strings.HasPrefix(rel, ".panel") {
			return
		}
		if info, err := os.Stat(abs); err == nil {
			if !info.IsDir() {
				return
			}
		} else if os.IsNotExist(err) {
			if looksLikeFile(rel) {
				return
			}
		} else {
			return
		}
		t := strings.TrimSpace(target)
		if t != "" {
			for _, cur := range mountTargets[rel] {
				if cur == t {
					return
				}
			}
			mountTargets[rel] = append(mountTargets[rel], t)
		} else if _, ok := mountTargets[rel]; !ok {
			mountTargets[rel] = nil
		}
	}

	// 1. Parse Compose YAML files for bind mounts.
	type yamlSrc struct {
		data    []byte
		baseDir string
	}
	var docs []yamlSrc
	if p.Source == "inline" && strings.TrimSpace(p.Compose) != "" {
		docs = append(docs, yamlSrc{data: []byte(p.Compose), baseDir: projDir})
	} else {
		cp := s.composeProject(p, false)
		for _, f := range cp.Files {
			if b, err := os.ReadFile(f); err == nil {
				docs = append(docs, yamlSrc{data: b, baseDir: cp.Dir})
			}
		}
		if len(docs) == 0 && strings.TrimSpace(p.Compose) != "" {
			docs = append(docs, yamlSrc{data: []byte(p.Compose), baseDir: projDir})
		}
	}
	for _, doc := range docs {
		extractComposeBindMounts(doc.data, func(src, target string) {
			addMount(src, target, doc.baseDir)
		})
	}

	// 2. Group multiple mounts sharing a common parent directory (e.g. data/* -> data).
	parentCounts := map[string]int{}
	mountAncestors := map[string]bool{}
	for rel := range mountTargets {
		for dir := path.Dir(rel); dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
			parentCounts[dir]++
			mountAncestors[dir] = true
		}
	}
	var promoted []string
	for dir, count := range parentCounts {
		if count < 2 {
			continue
		}
		if _, exists := mountTargets[dir]; exists {
			continue
		}
		// Skip outer ancestor when a deeper child covers the exact same mounts.
		redundant := false
		prefix := dir + "/"
		for other, oc := range parentCounts {
			if oc == count && strings.HasPrefix(other, prefix) {
				redundant = true
				break
			}
		}
		if !redundant {
			promoted = append(promoted, dir)
		}
	}
	sort.Slice(promoted, func(i, j int) bool {
		if parentCounts[promoted[i]] != parentCounts[promoted[j]] {
			return parentCounts[promoted[i]] > parentCounts[promoted[j]]
		}
		return promoted[i] < promoted[j]
	})

	out := make([]backupDirOption, 0, len(promoted)+len(mountTargets))
	seen := map[string]bool{}
	for _, dir := range promoted {
		seen[dir] = true
		out = append(out, backupDirOption{
			Path:  dir,
			Label: fmt.Sprintf("包含 %d 个挂载目录", parentCounts[dir]),
			Hint:  "推荐",
		})
	}

	mountPaths := make([]string, 0, len(mountTargets))
	for rel := range mountTargets {
		mountPaths = append(mountPaths, rel)
	}
	sort.Strings(mountPaths)
	for _, rel := range mountPaths {
		seen[rel] = true
		targets := mountTargets[rel]
		sort.Strings(targets)
		label := "Compose 挂载目录"
		if len(targets) > 0 {
			label = "挂载 → " + strings.Join(targets, ", ")
		}
		out = append(out, backupDirOption{Path: rel, Label: label, Hint: "挂载"})
	}

	// 3. Scan existing top-level directories on disk not already covered by mounts.
	scanBase := projDir
	scanPrefix := ""
	if p.Source == "git" {
		if info, err := os.Stat(filepath.Join(projDir, "src")); err == nil && info.IsDir() {
			scanBase = filepath.Join(projDir, "src")
			scanPrefix = "src/"
		}
	}
	if entries, err := os.ReadDir(scanBase); err == nil {
		var diskDirs []string
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if strings.HasPrefix(name, ".") || ignoredScanDirs[name] {
				continue
			}
			rel := scanPrefix + name
			if seen[rel] || mountAncestors[rel] {
				continue
			}
			diskDirs = append(diskDirs, rel)
		}
		sort.Strings(diskDirs)
		for _, rel := range diskDirs {
			seen[rel] = true
			out = append(out, backupDirOption{Path: rel, Label: "项目子目录", Hint: "目录"})
		}
	}

	return out
}

func extractComposeBindMounts(data []byte, fn func(source, target string)) {
	var doc struct {
		Services map[string]struct {
			Volumes []any `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return
	}
	for _, svc := range doc.Services {
		for _, item := range svc.Volumes {
			switch v := item.(type) {
			case string:
				parts := strings.Split(v, ":")
				if len(parts) >= 2 {
					fn(parts[0], parts[1])
				}
			case map[string]any:
				typ, _ := v["type"].(string)
				if strings.ToLower(strings.TrimSpace(typ)) != "bind" {
					continue
				}
				src, _ := v["source"].(string)
				dst, _ := v["target"].(string)
				if src != "" {
					fn(src, dst)
				}
			}
		}
	}
}

func expandComposeEnv(s string, env map[string]string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	return composeVarRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := composeVarRe.FindStringSubmatch(m)
		key := sub[1]
		def := sub[2]
		if key == "" {
			key = sub[3]
		}
		if v, ok := env[key]; ok && v != "" {
			return v
		}
		if def != "" {
			return def
		}
		return m
	})
}

func isBindSource(s string) bool {
	return s == "." || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") ||
		strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~/") || strings.Contains(s, "/")
}

func looksLikeFile(rel string) bool {
	ext := strings.ToLower(path.Ext(path.Base(rel)))
	return likelyFileExts[ext]
}
