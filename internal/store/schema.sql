CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
  id             INTEGER PRIMARY KEY,
  username       TEXT NOT NULL UNIQUE,
  email          TEXT NOT NULL DEFAULT '',
  password_hash  TEXT NOT NULL,
  role           TEXT NOT NULL,              -- admin | operator | viewer
  totp_secret    TEXT NOT NULL DEFAULT '',   -- encrypted
  totp_enabled   INTEGER NOT NULL DEFAULT 0,
  last_totp_step INTEGER NOT NULL DEFAULT 0,
  recovery_codes TEXT NOT NULL DEFAULT '',   -- newline separated sha256 hashes
  disabled       INTEGER NOT NULL DEFAULT 0,
  all_projects   INTEGER NOT NULL DEFAULT 1,
  created_at     INTEGER NOT NULL,
  last_login_at  INTEGER NOT NULL DEFAULT 0,
  last_login_ip  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS user_projects (
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  PRIMARY KEY (user_id, project_id)
);

CREATE TABLE IF NOT EXISTS sessions (
  id           TEXT PRIMARY KEY,             -- sha256 of cookie token
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  mfa_pending  INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  ip           TEXT NOT NULL DEFAULT '',
  user_agent   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS api_tokens (
  id           INTEGER PRIMARY KEY,
  name         TEXT NOT NULL,
  prefix       TEXT NOT NULL,
  token_hash   TEXT NOT NULL UNIQUE,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  scope        TEXT NOT NULL,                -- read | deploy | full
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL DEFAULT 0,
  last_used_at INTEGER NOT NULL DEFAULT 0,
  last_used_ip TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS projects (
  id               INTEGER PRIMARY KEY,
  name             TEXT NOT NULL UNIQUE,
  description      TEXT NOT NULL DEFAULT '',
  kind             TEXT NOT NULL DEFAULT 'app',     -- app | database
  source           TEXT NOT NULL DEFAULT 'inline',  -- inline | git | imported
  workdir          TEXT NOT NULL DEFAULT '',        -- only for imported projects
  git_url          TEXT NOT NULL DEFAULT '',
  git_branch       TEXT NOT NULL DEFAULT 'main',
  git_compose_path TEXT NOT NULL DEFAULT 'compose.yml',
  git_key          TEXT NOT NULL DEFAULT '',        -- encrypted private key
  webhook_token    TEXT NOT NULL DEFAULT '',
  webhook_secret   TEXT NOT NULL DEFAULT '',        -- encrypted
  auto_deploy      INTEGER NOT NULL DEFAULT 0,
  compose          TEXT NOT NULL DEFAULT '',
  env              TEXT NOT NULL DEFAULT '',        -- encrypted JSON [{key,value,secret}]
  options          TEXT NOT NULL DEFAULT '{}',      -- JSON deploy options
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_projects_webhook ON projects(webhook_token);

CREATE TABLE IF NOT EXISTS project_files (
  project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  path       TEXT NOT NULL,
  content    TEXT NOT NULL,
  PRIMARY KEY (project_id, path)
);

CREATE TABLE IF NOT EXISTS deployments (
  id          INTEGER PRIMARY KEY,
  project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  number      INTEGER NOT NULL,
  status      TEXT NOT NULL,                 -- queued | running | success | failed | rolled_back | canceled
  trigger     TEXT NOT NULL,                 -- manual | webhook | rollback | create
  actor       TEXT NOT NULL DEFAULT '',
  commit_sha  TEXT NOT NULL DEFAULT '',
  commit_msg  TEXT NOT NULL DEFAULT '',
  compose     TEXT NOT NULL DEFAULT '',
  env         TEXT NOT NULL DEFAULT '',      -- encrypted snapshot
  files_json  TEXT NOT NULL DEFAULT '',      -- inline project_files snapshot
  job_id      TEXT NOT NULL DEFAULT '',
  error       TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS deployments_project ON deployments(project_id, number DESC);

CREATE TABLE IF NOT EXISTS db_instances (
  id            INTEGER PRIMARY KEY,
  project_id    INTEGER NOT NULL UNIQUE REFERENCES projects(id) ON DELETE CASCADE,
  engine        TEXT NOT NULL,               -- postgres | mysql | mariadb | mongo | redis
  version       TEXT NOT NULL,
  root_user     TEXT NOT NULL,
  root_password TEXT NOT NULL,               -- encrypted
  port          INTEGER NOT NULL,
  exposure      TEXT NOT NULL DEFAULT 'internal', -- internal | local | public
  created_at    INTEGER NOT NULL,
  image         TEXT NOT NULL DEFAULT ''        -- custom image; '' = official image for engine+version
);

CREATE TABLE IF NOT EXISTS registries (
  id       INTEGER PRIMARY KEY,
  host     TEXT NOT NULL UNIQUE,
  username TEXT NOT NULL,
  password TEXT NOT NULL                     -- encrypted
);

CREATE TABLE IF NOT EXISTS storage_targets (
  id             INTEGER PRIMARY KEY,
  name           TEXT NOT NULL UNIQUE,
  kind           TEXT NOT NULL,              -- local | s3
  config         TEXT NOT NULL,              -- encrypted JSON
  created_at     INTEGER NOT NULL,
  last_check_at  INTEGER NOT NULL DEFAULT 0,
  last_check_ok  INTEGER NOT NULL DEFAULT 0,
  last_check_msg TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS backup_policies (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL,
  source        TEXT NOT NULL,               -- JSON {kind, instanceId, database, volume, projectId, path}
  schedule      TEXT NOT NULL,               -- cron expression
  keep_daily    INTEGER NOT NULL DEFAULT 7,
  keep_weekly   INTEGER NOT NULL DEFAULT 4,
  keep_monthly  INTEGER NOT NULL DEFAULT 6,
  keep_yearly   INTEGER NOT NULL DEFAULT 0,
  storage_id    INTEGER NOT NULL REFERENCES storage_targets(id),
  encrypt       INTEGER NOT NULL DEFAULT 1,
  age_recipient TEXT NOT NULL DEFAULT '',     -- extra recipients, one per line
  enabled       INTEGER NOT NULL DEFAULT 1,
  created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS backups (
  id           INTEGER PRIMARY KEY,
  policy_id    INTEGER REFERENCES backup_policies(id) ON DELETE SET NULL,
  source       TEXT NOT NULL,                -- JSON, same shape as policy source
  label        TEXT NOT NULL,
  storage_id   INTEGER NOT NULL,
  object_key   TEXT NOT NULL DEFAULT '',
  size         INTEGER NOT NULL DEFAULT 0,
  sha256       TEXT NOT NULL DEFAULT '',
  encrypted    INTEGER NOT NULL DEFAULT 0,
  format       TEXT NOT NULL DEFAULT '',
  status       TEXT NOT NULL,                -- running | success | failed
  error        TEXT NOT NULL DEFAULT '',
  tiers        TEXT NOT NULL DEFAULT '',
  job_id       TEXT NOT NULL DEFAULT '',
  started_at   INTEGER NOT NULL,
  finished_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS backups_policy ON backups(policy_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_backups_storage ON backups(storage_id);
CREATE INDEX IF NOT EXISTS idx_backups_started ON backups(started_at);

CREATE TABLE IF NOT EXISTS sites (
  id          INTEGER PRIMARY KEY,
  domains     TEXT NOT NULL,                 -- comma separated, first is primary
  kind        TEXT NOT NULL DEFAULT 'proxy', -- proxy | redirect
  project_id  INTEGER REFERENCES projects(id) ON DELETE SET NULL,
  service     TEXT NOT NULL DEFAULT '',
  port        INTEGER NOT NULL DEFAULT 0,
  upstream    TEXT NOT NULL DEFAULT '',      -- for arbitrary upstream or redirect target
  hsts        INTEGER NOT NULL DEFAULT 0,
  compress    INTEGER NOT NULL DEFAULT 1,
  basic_user  TEXT NOT NULL DEFAULT '',
  basic_hash  TEXT NOT NULL DEFAULT '',
  ip_allow    TEXT NOT NULL DEFAULT '',
  max_body    TEXT NOT NULL DEFAULT '',
  headers     TEXT NOT NULL DEFAULT '',      -- "Name: value" per line, added to upstream request
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS certificates (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  cert_pem   TEXT NOT NULL,
  key_pem    TEXT NOT NULL,                  -- encrypted
  domains    TEXT NOT NULL,
  not_before INTEGER NOT NULL,
  not_after  INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS jobs (
  id           TEXT PRIMARY KEY,
  kind         TEXT NOT NULL,
  title        TEXT NOT NULL,
  target       TEXT NOT NULL DEFAULT '',
  status       TEXT NOT NULL,                -- queued | running | success | failed | canceled
  progress     INTEGER NOT NULL DEFAULT 0,
  actor        TEXT NOT NULL DEFAULT '',
  error        TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  started_at   INTEGER NOT NULL DEFAULT 0,
  finished_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS jobs_created ON jobs(created_at DESC);

CREATE TABLE IF NOT EXISTS notify_channels (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL,
  kind        TEXT NOT NULL,                 -- webhook | telegram | email | wecom
  config      TEXT NOT NULL,                 -- encrypted JSON
  enabled     INTEGER NOT NULL DEFAULT 1,
  last_status TEXT NOT NULL DEFAULT '',
  last_sent_at INTEGER,
  created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS notify_rules (
  event      TEXT NOT NULL,
  channel_id INTEGER NOT NULL REFERENCES notify_channels(id) ON DELETE CASCADE,
  PRIMARY KEY (event, channel_id)
);

CREATE TABLE IF NOT EXISTS audit_log (
  id        INTEGER PRIMARY KEY,
  ts        INTEGER NOT NULL,
  actor     TEXT NOT NULL,
  action    TEXT NOT NULL,
  target    TEXT NOT NULL DEFAULT '',
  ip        TEXT NOT NULL DEFAULT '',
  result    TEXT NOT NULL DEFAULT 'ok',
  detail    TEXT NOT NULL DEFAULT '',
  prev_hash TEXT NOT NULL DEFAULT '',
  hash      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS audit_ts ON audit_log(ts DESC);

-- ---------- phase 2 ----------

-- Per-minute resource samples (7 days). name is the container name, or "host".
CREATE TABLE IF NOT EXISTS metrics (
  ts       INTEGER NOT NULL,
  name     TEXT NOT NULL,
  project  TEXT NOT NULL DEFAULT '',
  cpu      REAL NOT NULL DEFAULT 0,          -- percent
  mem      INTEGER NOT NULL DEFAULT 0,       -- bytes used
  mem_limit INTEGER NOT NULL DEFAULT 0,
  net_rx   INTEGER NOT NULL DEFAULT 0,       -- cumulative counters
  net_tx   INTEGER NOT NULL DEFAULT 0,
  blk_r    INTEGER NOT NULL DEFAULT 0,
  blk_w    INTEGER NOT NULL DEFAULT 0,
  disk     INTEGER NOT NULL DEFAULT 0,       -- host only: used bytes
  PRIMARY KEY (name, ts)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS metrics_ts ON metrics(ts);
CREATE INDEX IF NOT EXISTS idx_metrics_project_ts ON metrics(project, ts);

CREATE TABLE IF NOT EXISTS templates (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  compose     TEXT NOT NULL,
  env         TEXT NOT NULL DEFAULT '',      -- encrypted JSON [{key,value,secret}]
  files       TEXT NOT NULL DEFAULT '',      -- encrypted JSON [{path,content}]
  service     TEXT NOT NULL DEFAULT '',
  port        INTEGER NOT NULL DEFAULT 0,
  created_by  TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS image_checks (
  project_id    INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  service       TEXT NOT NULL,
  image         TEXT NOT NULL,
  local_digest  TEXT NOT NULL DEFAULT '',
  remote_digest    TEXT NOT NULL DEFAULT '',
  last_auto_digest TEXT NOT NULL DEFAULT '',
  status           TEXT NOT NULL,               -- current | update | local | error
  error         TEXT NOT NULL DEFAULT '',
  checked_at    INTEGER NOT NULL,
  PRIMARY KEY (project_id, service)
);

CREATE TABLE IF NOT EXISTS cron_tasks (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL,
  project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  service     TEXT NOT NULL,
  command     TEXT NOT NULL,
  user        TEXT NOT NULL DEFAULT '',
  schedule    TEXT NOT NULL,
  timeout     INTEGER NOT NULL DEFAULT 600,  -- seconds
  enabled     INTEGER NOT NULL DEFAULT 1,
  notify      INTEGER NOT NULL DEFAULT 1,    -- emit cron_failed on failure
  last_run_at INTEGER NOT NULL DEFAULT 0,
  last_status TEXT NOT NULL DEFAULT '',
  last_job_id TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS restore_drills (
  id          INTEGER PRIMARY KEY,
  policy_id   INTEGER NOT NULL REFERENCES backup_policies(id) ON DELETE CASCADE,
  schedule    TEXT NOT NULL,
  check_sql   TEXT NOT NULL DEFAULT '',      -- query run against the restored copy
  min_rows    INTEGER NOT NULL DEFAULT 0,    -- first cell of check_sql must be >= this
  enabled     INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS drill_runs (
  id          INTEGER PRIMARY KEY,
  drill_id    INTEGER REFERENCES restore_drills(id) ON DELETE SET NULL,
  backup_id   INTEGER NOT NULL,
  label       TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL,                 -- running | success | failed
  report      TEXT NOT NULL DEFAULT '',
  job_id      TEXT NOT NULL DEFAULT '',
  started_at  INTEGER NOT NULL,
  finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS drill_runs_started ON drill_runs(started_at DESC);
