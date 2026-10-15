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

var rlyEnv = []string{"RLY_CONFIG", "RLY_STAGING", "RLY_REMOTE", "RLY_REMOTE_PATH", "RLY_LOG_DIR", "RLY_LOG_TAIL_LINES", "RLY_SKIP_LOGS"}

func clearEnv(t *testing.T) {
	for _, k := range rlyEnv {
		t.Setenv(k, "")
	}
}

// newProject creates root/.rly with the given config and config.local.
func newProject(t *testing.T, config, local string) *Project {
	root := t.TempDir()
	p, err := initProject(root)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p.ConfigFile, []byte(config), 0o644)
	if local != "" {
		os.WriteFile(p.localConfigFile(), []byte(local), 0o644)
	}
	return p
}

func TestPrecedence(t *testing.T) {
	clearEnv(t)
	p := newProject(t, `
staging_dir = "out"
[remote]
target = "filehost"
path = "/file/path"
[logs]
dir = "logs"
tail_lines = 5
skip = ["a", "b"]
`, `
[remote]
target = "localhost"
[logs]
skip = ["c"]
`)
	t.Setenv("RLY_REMOTE", "envhost")
	t.Setenv("RLY_STAGING", "/from/env")
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Remote.Target != "localhost" || cfg.Remote.Path != "/file/path" || len(cfg.Logs.Skip) != 1 {
		t.Fatalf("config.local overlay wrong: %+v", cfg)
	}
	if err := cfg.applyOverrides(Overrides{Remote: "flaghost"}); err != nil {
		t.Fatal(err)
	}
	cfg.applyDefaults(p)
	if cfg.StagingDir != "/from/env" || cfg.Remote.Target != "flaghost" || cfg.Logs.TailLines != 5 {
		t.Fatalf("unexpected %+v", cfg)
	}
	if cfg.Logs.Dir != filepath.Join(p.Root, "logs") || cfg.Logs.Pattern != "*.log" {
		t.Fatalf("relative path/defaults wrong: %+v", cfg)
	}
}

func TestDefaults(t *testing.T) {
	clearEnv(t)
	p := newProject(t, "", "")
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	cfg.applyDefaults(p)
	if cfg.StagingDir != filepath.Join(p.Root, ".rly", "staging") {
		t.Errorf("staging = %s", cfg.StagingDir)
	}
	if cfg.Remote.Path != "~/.rly/inbox/"+filepath.Base(p.Root) {
		t.Errorf("remote path = %s", cfg.Remote.Path)
	}
}

func TestNoProject(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	err := run([]string{"bundle", "--no-push"})
	if err == nil || !strings.Contains(err.Error(), "rly init") {
		t.Fatalf("want init hint, got %v", err)
	}
}

func TestFindProjectFromSubdir(t *testing.T) {
	clearEnv(t)
	p := newProject(t, "", "")
	sub := filepath.Join(p.Root, "a", "b")
	os.MkdirAll(sub, 0o755)
	t.Chdir(sub)
	got, err := findProject("")
	if err != nil {
		t.Fatal(err)
	}
	// Compare resolved paths: TempDir may sit behind a symlink (macOS).
	want, _ := filepath.EvalSymlinks(p.Root)
	if have, _ := filepath.EvalSymlinks(got.Root); have != want {
		t.Fatalf("root = %s, want %s", got.Root, p.Root)
	}
}

func TestInit(t *testing.T) {
	clearEnv(t)
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, ".git"), 0o755)
	sub := filepath.Join(root, "pkg")
	os.Mkdir(sub, 0o755)
	t.Chdir(sub)
	if err := run([]string{"init"}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".rly/config", ".rly/.gitignore", ".rly/staging"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("init should create %s at the git root: %v", f, err)
		}
	}
	cfgFile := filepath.Join(root, ".rly", "config")
	os.WriteFile(cfgFile, []byte("# mine\n"), 0o644)
	if err := run([]string{"init"}); err == nil {
		t.Fatal("second init should refuse")
	}
	if b, _ := os.ReadFile(cfgFile); string(b) != "# mine\n" {
		t.Fatal("init overwrote existing config")
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
	clearEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/broken" {
			http.Error(w, "nope", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	remoteHome := t.TempDir()
	p := newProject(t, fmt.Sprintf(`
[remote]
target = "box"
path = "~/inbox"
[logs]
dir = "logs"
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
command = "cat data/app.db > {out}"
output = "app.db"
[[snapshot]]
command = "exit 3"
output = "fails.db"
`, srv.URL, srv.URL), fmt.Sprintf("[remote]\nssh = %q\n", fakeSSH(t, remoteHome)))
	root := p.Root
	staging := filepath.Join(p.RlyDir, "staging")
	logs := filepath.Join(root, "logs")
	os.MkdirAll(logs, 0o755)
	os.MkdirAll(filepath.Join(root, "data"), 0o755)
	os.WriteFile(filepath.Join(root, "data", "app.db"), []byte("dbdata"), 0o644)
	os.WriteFile(filepath.Join(logs, "server.log"), []byte("1\n2\n3\n4\n"), 0o644)
	os.WriteFile(filepath.Join(logs, "noisy.log"), []byte("x\n"), 0o644)
	os.WriteFile(filepath.Join(logs, "notes.txt"), []byte("x\n"), 0o644)

	t.Chdir(logs) // discovery walks up; relative paths resolve from root
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
	if local, _ := filepath.Glob(filepath.Join(staging, "*-out.txt")); len(local) != 0 {
		t.Error("single push must not touch staging")
	}
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

func TestInitOutsideGit(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	if err := run([]string{"init"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".rly", "config")); err != nil {
		t.Fatal("init outside git should use the current directory")
	}
}

func TestPushWithoutProject(t *testing.T) {
	clearEnv(t)
	remoteHome := t.TempDir()
	dir := t.TempDir()
	t.Chdir(dir)
	src := filepath.Join(dir, "out.txt")
	os.WriteFile(src, []byte("hi"), 0o644)

	if err := run([]string{"push", src}); err == nil || !strings.Contains(err.Error(), "--remote") {
		t.Fatalf("want hint about --remote, got %v", err)
	}
	// No project means no config to name the fake ssh, so put it on PATH.
	bin := t.TempDir()
	os.Symlink(fakeSSH(t, remoteHome), filepath.Join(bin, "ssh"))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := run([]string{"push", src, "--remote", "box"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := filepath.Glob(filepath.Join(remoteHome, ".rly", "inbox", "*-out.txt")); len(got) != 1 {
		t.Fatalf("expected file in ~/.rly/inbox, got %v", got)
	}
}
