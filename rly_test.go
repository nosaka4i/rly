package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTailOffset(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"", 3, ""},
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\nc", 2, "b\nc"},
		{"a\nb\nc\n", 5, "a\nb\nc\n"},
		{"a\nb\nc\n", 3, "a\nb\nc\n"},
		{"\n\n\n", 1, "\n"},
	}
	for _, c := range cases {
		off, err := tailOffset(strings.NewReader(c.in), int64(len(c.in)), c.n)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.in[off:]; got != c.want {
			t.Errorf("tail(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestTailFileAcrossChunks(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50000; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	p := filepath.Join(t.TempDir(), "x.log")
	os.WriteFile(p, []byte(b.String()), 0o644)
	var out bytes.Buffer
	dropped, err := tailFile(p, &out, 10000)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 10000 || lines[0] != "line 40000" || lines[9999] != "line 49999" {
		t.Fatalf("got %d lines, first %q", len(lines), lines[0])
	}
	if dropped == 0 {
		t.Fatal("expected dropped bytes")
	}
}

func TestSkipMatch(t *testing.T) {
	skip := []string{"ollama", "debug-*.log", "x.log"}
	for name, want := range map[string]bool{
		"ollama.log": true, "debug-1.log": true, "x.log": true, "server.log": false,
	} {
		if got := skipMatch(name, skip); got != want {
			t.Errorf("skipMatch(%q) = %v", name, got)
		}
	}
}

func TestRemoteDirExpr(t *testing.T) {
	for in, want := range map[string]string{
		"~":         `"$HOME"`,
		"~/a b":     `"$HOME"/'a b'`,
		"/srv/it's": `'/srv/it'\''s'`,
		"rel/dir":   `'rel/dir'`,
	} {
		if got := remoteDirExpr(in); got != want {
			t.Errorf("remoteDirExpr(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestPrecedence(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config")
	os.WriteFile(cfgPath, []byte(`
staging_dir = "/from/file"
[remote]
target = "filehost"
[logs]
tail_lines = 5
`), 0o644)
	t.Setenv("RLY_HOME", "/from/env")
	t.Setenv("RLY_REMOTE", "envhost")
	t.Setenv("RLY_LOG_TAIL_LINES", "")
	cfg, _, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.applyOverrides(Overrides{Remote: "flaghost"}); err != nil {
		t.Fatal(err)
	}
	cfg.applyDefaults()
	if cfg.StagingDir != "/from/env" || cfg.Remote.Target != "flaghost" || cfg.Logs.TailLines != 5 {
		t.Fatalf("unexpected %+v", cfg)
	}
	if cfg.Remote.Path != defaultRemotePath || cfg.Logs.Pattern != "*.log" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestStagingDirRequired(t *testing.T) {
	t.Setenv("RLY_HOME", "")
	t.Setenv("RLY_CONFIG", filepath.Join(t.TempDir(), "missing"))
	err := run([]string{"bundle", "--no-push"})
	if err == nil || !strings.Contains(err.Error(), "rly init") {
		t.Fatalf("want init hint, got %v", err)
	}
}

func TestInitPreservesConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "sub", "config")
	if err := writeStagingDir(cfgPath, "/one"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cfgPath)
	edited := strings.Replace(string(data), `target = ""`, `target = "box" # mine`, 1)
	os.WriteFile(cfgPath, []byte(edited), 0o644)
	if err := writeStagingDir(cfgPath, "/two"); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StagingDir != "/two" || cfg.Remote.Target != "box" {
		t.Fatalf("got %+v", cfg)
	}
	data, _ = os.ReadFile(cfgPath)
	if !strings.Contains(string(data), "# mine") {
		t.Fatal("comments lost")
	}
}

// fakeSSH writes a script that ignores the host and runs the remote command
// locally with HOME pointed at remoteHome.
func fakeSSH(t *testing.T, remoteHome string) string {
	p := filepath.Join(t.TempDir(), "fakessh")
	script := fmt.Sprintf("#!/bin/sh\nshift\nHOME=%s exec sh -c \"$1\"\n", shellQuote(remoteHome))
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBundleEndToEnd(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	logs := filepath.Join(root, "logs")
	remoteHome := filepath.Join(root, "remote")
	os.MkdirAll(staging, 0o755)
	os.MkdirAll(logs, 0o755)
	os.MkdirAll(remoteHome, 0o755)
	os.WriteFile(filepath.Join(logs, "server.log"), []byte("1\n2\n3\n4\n"), 0o644)
	os.WriteFile(filepath.Join(logs, "noisy.log"), []byte("x\n"), 0o644)
	os.WriteFile(filepath.Join(logs, "notes.txt"), []byte("x\n"), 0o644)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/broken" {
			http.Error(w, "nope", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	cfgPath := filepath.Join(root, "config")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf(`
staging_dir = %q
[remote]
target = "box"
path = "~/inbox"
ssh = %q
[logs]
dir = %q
tail_lines = 2
skip = ["noisy"]
[[http]]
name = "health"
url = "%s/health"
[[http]]
name = "broken"
url = "%s/broken"
[[http]]
name = "down"
url = "http://127.0.0.1:1/x"
[[snapshot]]
name = "db"
command = "printf dbdata > {out}"
output = "app.db"
[[snapshot]]
command = "exit 3"
output = "fails.db"
`, staging, fakeSSH(t, remoteHome), logs, srv.URL, srv.URL)), 0o644)

	for _, k := range []string{"RLY_HOME", "RLY_REMOTE", "RLY_REMOTE_PATH", "RLY_LOG_DIR", "RLY_LOG_TAIL_LINES", "RLY_SKIP_LOGS"} {
		t.Setenv(k, "")
	}
	t.Setenv("RLY_CONFIG", cfgPath)
	if err := run([]string{"bundle"}); err != nil {
		t.Fatal(err)
	}

	pushed, _ := filepath.Glob(filepath.Join(remoteHome, "inbox", "*-bundle.tar.gz"))
	if len(pushed) != 1 {
		t.Fatalf("expected one pushed bundle, got %v", pushed)
	}
	files := readTarGz(t, pushed[0])
	name := strings.TrimSuffix(filepath.Base(pushed[0]), ".tar.gz")
	get := func(p string) string { return files[name+"/"+p] }

	if got := get("logs/server.log"); got != "3\n4\n" {
		t.Errorf("server.log = %q", got)
	}
	if _, ok := files[name+"/logs/noisy.log"]; ok {
		t.Error("noisy.log should be skipped")
	}
	if _, ok := files[name+"/logs/notes.txt"]; ok {
		t.Error("notes.txt should not match pattern")
	}
	if get("http/health.json") != `{"ok":true}` {
		t.Errorf("health = %q", get("http/health.json"))
	}
	if !strings.Contains(get("http/broken.txt"), "nope") {
		t.Error("broken body should be saved")
	}
	if get("snapshot/app.db") != "dbdata" {
		t.Errorf("snapshot = %q", get("snapshot/app.db"))
	}
	man := get("MANIFEST.md")
	for _, want := range []string{"http/down", "fails.db", "500", "noisy.log: excluded"} {
		if !strings.Contains(man, want) {
			t.Errorf("manifest missing %q:\n%s", want, man)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(staging, ".rly-tmp-*")); len(left) != 0 {
		t.Errorf("temp dirs left behind: %v", left)
	}

	// Single-file push mode.
	src := filepath.Join(root, "out.txt")
	os.WriteFile(src, []byte("hello"), 0o644)
	if err := run([]string{"push", src, "--remote-path", "~/single"}); err != nil {
		t.Fatal(err)
	}
	got, _ := filepath.Glob(filepath.Join(remoteHome, "single", "*-out.txt"))
	if len(got) != 1 {
		t.Fatalf("single push: %v", got)
	}
	if b, _ := os.ReadFile(got[0]); string(b) != "hello" {
		t.Fatalf("single push content %q", b)
	}
	if _, err := time.Parse(timestampLayout, strings.TrimSuffix(filepath.Base(got[0]), "-out.txt")); err != nil {
		t.Errorf("bad timestamp prefix: %v", err)
	}
}

func readTarGz(t *testing.T, path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			out[h.Name] = string(b)
		}
	}
}
