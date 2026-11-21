package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// GDriveScope grants access to folders and files in the user's Google Drive,
	// including pre-existing folders referenced by Folder ID.
	GDriveScope = "https://www.googleapis.com/auth/drive"

	defaultGDriveAuthURL   = "https://accounts.google.com/o/oauth2/v2/auth"
	defaultGDriveTokenURL  = "https://oauth2.googleapis.com/token"
	defaultGDriveAPIBase   = "https://www.googleapis.com/drive/v3"
	defaultGDriveUpload    = "https://www.googleapis.com/upload/drive/v3"
	defaultGDriveChunkSize = 8 << 20 // 8 MiB (must be a multiple of 256 KiB)
	driveFolderMime        = "application/vnd.google-apps.folder"
)

var (
	driveFolderURLRe = regexp.MustCompile(`/folders/([a-zA-Z0-9_-]+)`)
)

// NormalizeGDriveFolderID accepts either a raw folder ID or a pasted Google Drive folder URL
// and returns the extracted folder ID (or empty string if blank).
func NormalizeGDriveFolderID(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		if m := driveFolderURLRe.FindStringSubmatch(u.Path); len(m) == 2 {
			return m[1]
		}
		if id := strings.TrimSpace(u.Query().Get("id")); id != "" {
			return id
		}
	}
	return strings.Trim(s, "/")
}

// GDriveAuthURL builds the Google OAuth 2.0 consent URL requesting an offline refresh token.
func GDriveAuthURL(clientID, redirectURI, state, codeChallenge string) string {
	q := url.Values{
		"client_id":             {strings.TrimSpace(clientID)},
		"redirect_uri":          {strings.TrimSpace(redirectURI)},
		"response_type":         {"code"},
		"scope":                 {GDriveScope},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
	}
	return defaultGDriveAuthURL + "?" + q.Encode()
}

// GDriveExchangeCode exchanges an OAuth 2.0 authorization code for a refresh token.
func GDriveExchangeCode(ctx context.Context, clientID, clientSecret, code, redirectURI, codeVerifier string) (string, error) {
	return gdriveExchangeCode(ctx, defaultGDriveTokenURL, clientID, clientSecret, code, redirectURI, codeVerifier)
}

func gdriveExchangeCode(ctx context.Context, tokenURL, clientID, clientSecret, code, redirectURI, codeVerifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {strings.TrimSpace(code)},
		"client_id":     {strings.TrimSpace(clientID)},
		"client_secret": {strings.TrimSpace(clientSecret)},
		"redirect_uri":  {strings.TrimSpace(redirectURI)},
	}
	if codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("连接 Google 授权服务器失败：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return "", parseOAuthError(resp.StatusCode, body)
	}
	var out struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("解析 Google 授权响应失败：%w", err)
	}
	if strings.TrimSpace(out.RefreshToken) == "" {
		return "", fmt.Errorf("Google 未返回 Refresh Token，请移除已授权应用后重试")
	}
	return out.RefreshToken, nil
}

// GDrive stores backup objects in a personal Google Drive using OAuth 2.0 and Drive API v3.
type GDrive struct {
	clientID     string
	clientSecret string
	refreshToken string
	folderID     string // normalized explicit folder ID; empty means "root"
	prefix       string

	tokenURL   string
	apiBase    string
	uploadBase string
	chunkSize  int
	hc         *http.Client

	tokMu    sync.Mutex
	tok      string
	tokUntil time.Time

	dirMu sync.Mutex
}

func NewGDrive(c TargetConfig) (*GDrive, error) {
	cid := strings.TrimSpace(c.ClientID)
	csec := strings.TrimSpace(c.ClientSecret)
	rtok := strings.TrimSpace(c.RefreshToken)
	if cid == "" || csec == "" {
		return nil, fmt.Errorf("Client ID 和 Client Secret 不能为空")
	}
	if rtok == "" {
		return nil, fmt.Errorf("Refresh Token 不能为空，请先完成 Google 授权")
	}
	return &GDrive{
		clientID:     cid,
		clientSecret: csec,
		refreshToken: rtok,
		folderID:     NormalizeGDriveFolderID(c.FolderID),
		prefix:       strings.Trim(strings.ReplaceAll(c.Prefix, "\\", "/"), "/"),
		tokenURL:     defaultGDriveTokenURL,
		apiBase:      defaultGDriveAPIBase,
		uploadBase:   defaultGDriveUpload,
		chunkSize:    defaultGDriveChunkSize,
		hc:           &http.Client{},
	}, nil
}

func (g *GDrive) rootID() string {
	if g.folderID == "" {
		return "root"
	}
	return g.folderID
}

func (g *GDrive) accessToken(ctx context.Context, forceRefresh bool) (string, error) {
	g.tokMu.Lock()
	defer g.tokMu.Unlock()
	if !forceRefresh && g.tok != "" && time.Now().Before(g.tokUntil) {
		return g.tok, nil
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {g.clientID},
		"client_secret": {g.clientSecret},
		"refresh_token": {g.refreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("刷新 Google 访问令牌失败：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return "", parseOAuthError(resp.StatusCode, body)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("Google 访问令牌响应无效")
	}
	ttl := time.Duration(out.ExpiresIn)*time.Second - time.Minute
	if ttl < 30*time.Second {
		ttl = 30 * time.Second
	}
	g.tok = out.AccessToken
	g.tokUntil = time.Now().Add(ttl)
	return g.tok, nil
}

func parseOAuthError(status int, body []byte) error {
	var e struct {
		Err  string `json:"error"`
		Desc string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &e)
	switch e.Err {
	case "invalid_grant":
		return fmt.Errorf("Google 授权已失效或被撤销，请重新授权")
	case "invalid_client":
		return fmt.Errorf("Client ID 或 Client Secret 无效")
	}
	if e.Desc != "" {
		return fmt.Errorf("Google 认证失败（%d）：%s", status, e.Desc)
	}
	if e.Err != "" {
		return fmt.Errorf("Google 认证失败（%d）：%s", status, e.Err)
	}
	return fmt.Errorf("Google 认证失败（%d）", status)
}

func parseDriveError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	resp.Body.Close()
	var e struct {
		Err struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil {
		if len(e.Err.Errors) > 0 && e.Err.Errors[0].Reason == "storageQuotaExceeded" {
			return fmt.Errorf("Google Drive 存储空间不足")
		}
		if e.Err.Message != "" {
			return fmt.Errorf("Google Drive %d：%s", resp.StatusCode, e.Err.Message)
		}
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	return fmt.Errorf("Google Drive %d：%s", resp.StatusCode, msg)
}

func (g *GDrive) do(ctx context.Context, method, rawURL string, body []byte, hdr map[string]string) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := g.accessToken(ctx, attempt > 0)
		if err != nil {
			return nil, err
		}
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, r)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := g.hc.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			continue
		}
		return resp, nil
	}
	return nil, fmt.Errorf("Google Drive 认证失败")
}

func escapeDriveQuery(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return s
}

func (g *GDrive) splitKey(key string) ([]string, string, error) {
	raw := strings.ReplaceAll(key, "\\", "/")
	if g.prefix != "" {
		raw = g.prefix + "/" + strings.TrimLeft(raw, "/")
	}
	cleaned := strings.Trim(path.Clean("/"+raw), "/")
	if cleaned == "" || cleaned == "." {
		return nil, "", fmt.Errorf("无效的对象路径")
	}
	parts := strings.Split(cleaned, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return nil, "", fmt.Errorf("无效的对象路径")
		}
	}
	return parts[:len(parts)-1], parts[len(parts)-1], nil
}

type driveFileEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Trashed  bool   `json:"trashed"`
}

func (g *GDrive) queryFiles(ctx context.Context, q string) ([]driveFileEntry, error) {
	u := g.apiBase + "/files?" + url.Values{
		"q":        {q},
		"spaces":   {"drive"},
		"fields":   {"files(id,name,mimeType)"},
		"orderBy":  {"modifiedTime desc"},
		"pageSize": {"100"},
	}.Encode()
	resp, err := g.do(ctx, http.MethodGet, u, nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, parseDriveError(resp)
	}
	defer resp.Body.Close()
	var out struct {
		Files []driveFileEntry `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Files, nil
}

func (g *GDrive) findFolder(ctx context.Context, parentID, name string) (string, bool, error) {
	q := fmt.Sprintf("name = '%s' and '%s' in parents and mimeType = '%s' and trashed = false",
		escapeDriveQuery(name), escapeDriveQuery(parentID), driveFolderMime)
	files, err := g.queryFiles(ctx, q)
	if err != nil {
		return "", false, err
	}
	if len(files) == 0 {
		return "", false, nil
	}
	return files[0].ID, true, nil
}

func (g *GDrive) createFolder(ctx context.Context, parentID, name string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"name":     name,
		"mimeType": driveFolderMime,
		"parents":  []string{parentID},
	})
	resp, err := g.do(ctx, http.MethodPost, g.apiBase+"/files?fields=id", payload, map[string]string{
		"Content-Type": "application/json; charset=UTF-8",
	})
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", parseDriveError(resp)
	}
	defer resp.Body.Close()
	var out driveFileEntry
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (g *GDrive) ensureDir(ctx context.Context, dirs []string) (string, error) {
	parent := g.rootID()
	if len(dirs) == 0 {
		return parent, nil
	}
	g.dirMu.Lock()
	defer g.dirMu.Unlock()
	for _, name := range dirs {
		id, ok, err := g.findFolder(ctx, parent, name)
		if err != nil {
			return "", err
		}
		if !ok {
			id, err = g.createFolder(ctx, parent, name)
			if err != nil {
				return "", err
			}
		}
		parent = id
	}
	return parent, nil
}

func (g *GDrive) findDir(ctx context.Context, dirs []string) (string, bool, error) {
	parent := g.rootID()
	for _, name := range dirs {
		id, ok, err := g.findFolder(ctx, parent, name)
		if err != nil || !ok {
			return "", false, err
		}
		parent = id
	}
	return parent, true, nil
}

func (g *GDrive) findFiles(ctx context.Context, parentID, name string) ([]driveFileEntry, error) {
	q := fmt.Sprintf("name = '%s' and '%s' in parents and mimeType != '%s' and trashed = false",
		escapeDriveQuery(name), escapeDriveQuery(parentID), driveFolderMime)
	return g.queryFiles(ctx, q)
}

func (g *GDrive) initiateResumable(ctx context.Context, parentID, fileName, existingID string) (string, error) {
	method := http.MethodPost
	u := g.uploadBase + "/files?uploadType=resumable"
	meta := map[string]any{"name": fileName, "parents": []string{parentID}}
	if existingID != "" {
		method = http.MethodPatch
		u = g.uploadBase + "/files/" + url.PathEscape(existingID) + "?uploadType=resumable"
		meta = map[string]any{"name": fileName}
	}
	payload, _ := json.Marshal(meta)
	resp, err := g.do(ctx, method, u, payload, map[string]string{
		"Content-Type":          "application/json; charset=UTF-8",
		"X-Upload-Content-Type": "application/octet-stream",
	})
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", parseDriveError(resp)
	}
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if loc == "" {
		return "", fmt.Errorf("Google Drive 未返回上传会话地址")
	}
	return loc, nil
}

func (g *GDrive) uploadChunk(ctx context.Context, sessionURL string, data []byte, contentRange string) (int, error) {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := g.accessToken(ctx, attempt > 0)
		if err != nil {
			return 0, err
		}
		var body io.Reader
		if len(data) > 0 {
			body = bytes.NewReader(data)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, sessionURL, body)
		if err != nil {
			return 0, err
		}
		req.ContentLength = int64(len(data))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Content-Range", contentRange)
		resp, err := g.hc.Do(req)
		if err != nil {
			return 0, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			continue
		}
		if resp.StatusCode == 308 || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return resp.StatusCode, nil
		}
		return resp.StatusCode, parseDriveError(resp)
	}
	return 0, fmt.Errorf("Google Drive 上传认证失败")
}

func (g *GDrive) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	dirs, fileName, err := g.splitKey(key)
	if err != nil {
		return 0, err
	}
	parentID, err := g.ensureDir(ctx, dirs)
	if err != nil {
		return 0, err
	}
	existing, err := g.findFiles(ctx, parentID, fileName)
	if err != nil {
		return 0, err
	}
	existingID := ""
	if len(existing) > 0 {
		existingID = existing[0].ID
	}
	sessionURL, err := g.initiateResumable(ctx, parentID, fileName, existingID)
	if err != nil {
		return 0, err
	}

	chunkSize := g.chunkSize
	if chunkSize <= 0 {
		chunkSize = defaultGDriveChunkSize
	}
	// Read chunkSize+1 bytes so we always know whether EOF follows before emitting Content-Range.
	buf := make([]byte, chunkSize+1)
	var carried byte
	hasCarried := false
	var offset int64

	for {
		start := 0
		if hasCarried {
			buf[0] = carried
			start = 1
			hasCarried = false
		}
		n, rerr := io.ReadFull(r, buf[start:])
		avail := start + n
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			return offset, rerr
		}
		if avail == chunkSize+1 {
			carried = buf[chunkSize]
			hasCarried = true
			cr := fmt.Sprintf("bytes %d-%d/*", offset, offset+int64(chunkSize)-1)
			status, err := g.uploadChunk(ctx, sessionURL, buf[:chunkSize], cr)
			if err != nil {
				return offset, err
			}
			if status != 308 {
				return offset + int64(chunkSize), fmt.Errorf("Google Drive 分块上传状态异常：%d", status)
			}
			offset += int64(chunkSize)
			continue
		}
		// Final chunk (or empty stream).
		if avail == 0 && offset == 0 {
			if _, err := g.uploadChunk(ctx, sessionURL, nil, "bytes */0"); err != nil {
				return 0, err
			}
			return 0, nil
		}
		total := offset + int64(avail)
		cr := fmt.Sprintf("bytes %d-%d/%d", offset, total-1, total)
		if _, err := g.uploadChunk(ctx, sessionURL, buf[:avail], cr); err != nil {
			return offset, err
		}
		return total, nil
	}
}

func (g *GDrive) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	dirs, fileName, err := g.splitKey(key)
	if err != nil {
		return nil, err
	}
	parentID, ok, err := g.findDir(ctx, dirs)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("文件不存在：%s", key)
	}
	files, err := g.findFiles(ctx, parentID, fileName)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("文件不存在：%s", key)
	}
	u := g.apiBase + "/files/" + url.PathEscape(files[0].ID) + "?alt=media"
	resp, err := g.do(ctx, http.MethodGet, u, nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, parseDriveError(resp)
	}
	return resp.Body, nil
}

func (g *GDrive) Delete(ctx context.Context, key string) error {
	dirs, fileName, err := g.splitKey(key)
	if err != nil {
		return err
	}
	parentID, ok, err := g.findDir(ctx, dirs)
	if err != nil || !ok {
		return err
	}
	files, err := g.findFiles(ctx, parentID, fileName)
	if err != nil {
		return err
	}
	for _, f := range files {
		u := g.apiBase + "/files/" + url.PathEscape(f.ID)
		resp, err := g.do(ctx, http.MethodDelete, u, nil, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
			return parseDriveError(resp)
		}
		resp.Body.Close()
	}
	return nil
}

func (g *GDrive) Test(ctx context.Context) error {
	if _, err := g.accessToken(ctx, true); err != nil {
		return err
	}
	if g.folderID != "" {
		u := g.apiBase + "/files/" + url.PathEscape(g.folderID) + "?fields=id,name,mimeType,trashed"
		resp, err := g.do(ctx, http.MethodGet, u, nil, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return fmt.Errorf("目标文件夹不存在或无权访问")
		}
		if resp.StatusCode >= 300 {
			return parseDriveError(resp)
		}
		var meta driveFileEntry
		err = json.NewDecoder(resp.Body).Decode(&meta)
		resp.Body.Close()
		if err != nil {
			return err
		}
		if meta.Trashed {
			return fmt.Errorf("目标文件夹已在回收站中")
		}
		if meta.MimeType != driveFolderMime {
			return fmt.Errorf("指定的 Folder ID 不是文件夹")
		}
	}
	if _, err := g.Put(ctx, ".panel-write-test", strings.NewReader("ok")); err != nil {
		return fmt.Errorf("写入失败：%w", err)
	}
	return g.Delete(ctx, ".panel-write-test")
}
