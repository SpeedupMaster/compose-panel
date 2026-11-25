package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"filippo.io/age"

	"compose-panel/internal/auth"
)

// ---------- age backup keyring ----------

type extraAgeKey struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Identity  string `json:"identity,omitempty"`
	Recipient string `json:"recipient"`
	AddedAt   int64  `json:"addedAt"`
}

func (s *Server) loadExtraAgeKeys() []extraAgeKey {
	enc := s.setting("backup_age_extra_identities", "")
	if enc == "" {
		return nil
	}
	raw, err := s.box.Open(enc)
	if err != nil || raw == "" {
		return nil
	}
	var out []extraAgeKey
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

func (s *Server) saveExtraAgeKeys(keys []extraAgeKey) {
	if len(keys) == 0 {
		s.setSetting("backup_age_extra_identities", "")
		return
	}
	b, _ := json.Marshal(keys)
	s.setSetting("backup_age_extra_identities", s.box.Seal(string(b)))
}

// allAgeIdentities returns decryption identities in priority order:
// optional temporary key -> active primary panel key -> historical keyring.
func (s *Server) allAgeIdentities(tempKey string) ([]string, error) {
	var out []string
	if t := strings.TrimSpace(tempKey); t != "" {
		if _, err := age.ParseX25519Identity(t); err != nil {
			return nil, bad("临时 age 私钥格式无效：%v", err)
		}
		out = append(out, t)
	}
	if primary := strings.TrimSpace(s.box.MustOpen(s.setting("backup_age_identity", ""))); primary != "" {
		out = append(out, primary)
	}
	for _, k := range s.loadExtraAgeKeys() {
		if id := strings.TrimSpace(k.Identity); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

func (s *Server) handleBackupKey(w http.ResponseWriter, r *http.Request, u *User) error {
	extra := s.loadExtraAgeKeys()
	pubExtra := make([]extraAgeKey, 0, len(extra))
	for _, k := range extra {
		pubExtra = append(pubExtra, extraAgeKey{
			ID:        k.ID,
			Label:     k.Label,
			Recipient: k.Recipient,
			AddedAt:   k.AddedAt,
		})
	}
	out := map[string]any{
		"recipient": s.setting("backup_age_recipient", ""),
		"extraKeys": pubExtra,
	}
	if r.URL.Query().Get("reveal") == "1" {
		out["identity"] = s.box.MustOpen(s.setting("backup_age_identity", ""))
		s.audit(r, u, "backup.reveal_key", "", "查看备份私钥", nil)
	}
	return ok(w, out)
}

func (s *Server) handleImportBackupKey(w http.ResponseWriter, r *http.Request, u *User) error {
	var req struct {
		Label      string `json:"label"`
		Identity   string `json:"identity"`
		SetPrimary bool   `json:"setPrimary"`
	}
	if err := readJSON(r, &req); err != nil {
		return err
	}
	rawID := strings.TrimSpace(req.Identity)
	parsed, err := age.ParseX25519Identity(rawID)
	if err != nil {
		return bad("age 私钥格式无效（应以 AGE-SECRET-KEY-1 开头）")
	}
	rcp := parsed.Recipient().String()
	label := strings.TrimSpace(req.Label)
	if label == "" {
		label = "导入的私钥"
	}

	curRcp := strings.TrimSpace(s.setting("backup_age_recipient", ""))
	curID := strings.TrimSpace(s.box.MustOpen(s.setting("backup_age_identity", "")))
	extra := s.loadExtraAgeKeys()

	if req.SetPrimary {
		if rcp == curRcp {
			return bad("该私钥已是当前主密钥")
		}
		// Remove if already in historical list.
		kept := make([]extraAgeKey, 0, len(extra)+1)
		for _, k := range extra {
			if k.Recipient != rcp {
				kept = append(kept, k)
			}
		}
		// Preserve old primary in historical keyring so old backups remain decryptable.
		if curID != "" && curRcp != "" {
			exists := false
			for _, k := range kept {
				if k.Recipient == curRcp {
					exists = true
					break
				}
			}
			if !exists {
				kept = append(kept, extraAgeKey{
					ID:        auth.RandomToken(8),
					Label:     "原主密钥（自动归档）",
					Identity:  curID,
					Recipient: curRcp,
					AddedAt:   time.Now().Unix(),
				})
			}
		}
		s.saveExtraAgeKeys(kept)
		s.setSetting("backup_age_identity", s.box.Seal(rawID))
		s.setSetting("backup_age_recipient", rcp)
		s.audit(r, u, "backup.import_key", rcp, label+"（设为主密钥）", nil)
		return ok(w, map[string]any{"recipient": rcp, "primary": true})
	}

	if rcp == curRcp {
		return bad("该私钥已是当前主密钥，无需重复导入")
	}
	for _, k := range extra {
		if k.Recipient == rcp {
			return bad("该私钥已存在于历史解密密钥列表中")
		}
	}
	entry := extraAgeKey{
		ID:        auth.RandomToken(8),
		Label:     label,
		Identity:  rawID,
		Recipient: rcp,
		AddedAt:   time.Now().Unix(),
	}
	extra = append(extra, entry)
	s.saveExtraAgeKeys(extra)
	s.audit(r, u, "backup.import_key", rcp, label, nil)
	return ok(w, map[string]any{
		"id":        entry.ID,
		"label":     entry.Label,
		"recipient": entry.Recipient,
		"addedAt":   entry.AddedAt,
	})
}

func (s *Server) handleDeleteExtraBackupKey(w http.ResponseWriter, r *http.Request, u *User) error {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		return errNotFound
	}
	extra := s.loadExtraAgeKeys()
	kept := make([]extraAgeKey, 0, len(extra))
	var removed *extraAgeKey
	for i := range extra {
		if extra[i].ID == id {
			k := extra[i]
			removed = &k
			continue
		}
		kept = append(kept, extra[i])
	}
	if removed == nil {
		return errNotFound
	}
	s.saveExtraAgeKeys(kept)
	s.audit(r, u, "backup.delete_extra_key", removed.Recipient, removed.Label, nil)
	return ok(w, nil)
}
