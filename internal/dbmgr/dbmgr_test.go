package dbmgr

import (
	"strings"
	"testing"
)

func TestPostgresMajorFromImage(t *testing.T) {
	for in, want := range map[string]string{
		"postgres:18-alpine":                            "18",
		"postgres:17.2":                                 "17",
		"docker.io/library/postgres:16":                 "16",
		"pgvector/pgvector:pg17":                        "17",
		"pgvector/pgvector:0.8.0-pg18":                  "18",
		"postgis/postgis:17-3.5-alpine":                 "17",
		"timescale/timescaledb:2.17.2-pg16":             "16",
		"registry.local:5000/team/pg:15-ext":            "15",
		"postgres:latest":                               "",
		"postgres":                                      "",
		"registry.local:5000/postgres":                  "",
		"postgres:18@sha256:" + strings.Repeat("a", 64): "18",
	} {
		if got := PostgresMajorFromImage(in); got != want {
			t.Errorf("PostgresMajorFromImage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidImage(t *testing.T) {
	for in, want := range map[string]bool{
		"pgvector/pgvector:pg17":                       true,
		"registry.example.com:5000/team/postgres:17.2": true,
		"postgres@sha256:" + strings.Repeat("0", 64):   true,
		"Postgres:17":                       false,
		"postgres:17\n    privileged: true": false,
		"postgres:17 --flag":                false,
		"//evil":                            false,
		"-postgres":                         false,
		strings.Repeat("a", 256):            false,
	} {
		if got := ValidImage(in); got != want {
			t.Errorf("ValidImage(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestComposePostgresDataMount(t *testing.T) {
	cases := []struct{ version, image, wantImage, wantMount string }{
		{"17", "", "postgres:17-alpine", "data:/var/lib/postgresql/data\n"},
		{"18", "", "postgres:18-alpine", "data:/var/lib/postgresql\n"},
		{"18", "pgvector/pgvector:pg18", "pgvector/pgvector:pg18", "data:/var/lib/postgresql\n"},
		{"16", "postgis/postgis:16-3.5", "postgis/postgis:16-3.5", "data:/var/lib/postgresql/data\n"},
	}
	for _, c := range cases {
		out, err := Compose("postgres", c.version, c.image, "pg", "internal", "dbnet", 5432, "512m")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "    image: "+c.wantImage+"\n") || !strings.Contains(out, c.wantMount) {
			t.Errorf("version %s image %q:\n%s", c.version, c.image, out)
		}
		if strings.Count(out, "image:") != 1 {
			t.Errorf("expected exactly one image line:\n%s", out)
		}
	}
}

func TestComposeDefaultImages(t *testing.T) {
	for _, e := range Engines {
		out, err := Compose(e.ID, e.Versions[0], "", "x", "internal", "dbnet", e.Port, "")
		if err != nil {
			t.Fatal(err)
		}
		if want := "    image: " + e.DefaultImage(e.Versions[0]) + "\n"; !strings.Contains(out, want) || strings.Count(out, "image:") != 1 {
			t.Errorf("%s: want %q once in\n%s", e.ID, want, out)
		}
	}
	if _, err := Compose("oracle", "1", "", "x", "internal", "dbnet", 1, ""); err == nil {
		t.Error("unknown engine accepted")
	}
}

func TestMongoRestoreCmdNamespaceMapping(t *testing.T) {
	cl := Client{Engine: "mongo", Container: "mongo-1", Password: "pw"}

	// Explicit sourceDB maps sourceDB.* -> target.*
	cmd, err := cl.RestoreCmd("olddb", "newdb", true)
	if err != nil {
		t.Fatalf("RestoreCmd explicit sourceDB: %v", err)
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "--nsFrom olddb.*") || !strings.Contains(joined, "--nsTo newdb.*") || !strings.Contains(joined, "--drop") {
		t.Fatalf("unexpected explicit mongo restore args: %s", joined)
	}

	// Unknown/empty sourceDB maps $db$.$coll$ -> target.$coll$
	cmd2, err := cl.RestoreCmd("", "imported_db", false)
	if err != nil {
		t.Fatalf("RestoreCmd empty sourceDB: %v", err)
	}
	joined2 := strings.Join(cmd2.Args, " ")
	if !strings.Contains(joined2, "--nsFrom $db$.$coll$") || !strings.Contains(joined2, "--nsTo imported_db.$coll$") {
		t.Fatalf("unexpected wildcard mongo restore args: %s", joined2)
	}
}

func TestPostgresUserSchemasQueryEscapesPGPrefix(t *testing.T) {
	if strings.Contains(pgUserSchemasQuery, "LIKE 'pg_%'") {
		t.Fatalf("pgUserSchemasQuery uses unescaped LIKE 'pg_%%' wildcard: %s", pgUserSchemasQuery)
	}
	if !strings.Contains(pgUserSchemasQuery, `!~ '^pg_'`) {
		t.Fatalf("expected regex ^pg_ filter in pgUserSchemasQuery, got: %s", pgUserSchemasQuery)
	}
}
