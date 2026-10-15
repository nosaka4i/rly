package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"52428800": 52428800,
		"50M":      50 << 20,
		"50m":      50 << 20,
		"50MB":     50 << 20,
		"50MiB":    50 << 20,
		"512K":     512 << 10,
		"512kb":    512 << 10,
		"1G":       1 << 30,
		"1.5G":     3 << 29,
		" 2 M ":    2 << 20,
		"100B":     100,
	} {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "-1", "-5M", "abc", "5X", "1.5", "M"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) should fail", bad)
		}
	}
	for n, want := range map[int64]string{50 << 20: "50M", 3 << 29: "1.5G", 512: "512B", 1536: "1.5K"} {
		if got := formatSize(n); got != want {
			t.Errorf("formatSize(%d) = %s, want %s", n, got, want)
		}
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

var rlyEnv = []string{"RLY_CONFIG", "RLY_STAGING", "RLY_REMOTE", "RLY_REMOTE_PATH", "RLY_LOGS", "RLY_MAX_BUNDLE_BYTES"}

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
max_bundle_bytes = 1000
[remote]
target = "filehost"
path = "/file/path"
[[logs]]
path = "logs/*.log"
skip = ["a", "b"]
`, `
max_bundle_bytes = "2K"
[remote]
target = "localhost"
[[logs]]
path = "logs"
skip = ["c"]
`)
	t.Setenv("RLY_REMOTE", "envhost")
	t.Setenv("RLY_STAGING", "/from/env")
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Remote.Target != "localhost" || cfg.Remote.Path != "/file/path" || len(cfg.Logs) != 1 || len(cfg.Logs[0].Skip) != 1 || cfg.MaxBundleBytes != 2048 {
		t.Fatalf("config.local overlay wrong: %+v", cfg)
	}
	if err := cfg.applyOverrides(Overrides{Remote: "flaghost"}); err != nil {
		t.Fatal(err)
	}
	cfg.applyDefaults(p)
	if cfg.StagingDir != "/from/env" || cfg.Remote.Target != "flaghost" {
		t.Fatalf("unexpected %+v", cfg)
	}
	if cfg.Logs[0].Path != filepath.Join(p.Root, "logs") {
		t.Fatalf("relative log path not resolved from root: %+v", cfg.Logs)
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
	if len(cfg.Logs) != 0 {
		t.Errorf("there should be no default log source: %+v", cfg.Logs)
	}
	if cfg.Remote.Path != "~/.rly/inbox/"+filepath.Base(p.Root) {
		t.Errorf("remote path = %s", cfg.Remote.Path)
	}
	if cfg.MaxBundleBytes != 50<<20 {
		t.Errorf("max bundle bytes = %d", cfg.MaxBundleBytes)
	}
}

func TestMaxBundleBytesPrecedence(t *testing.T) {
	clearEnv(t)
	p := newProject(t, "max_bundle_bytes = \"1M\"\n", "")
	load := func(o Overrides) (ByteSize, error) {
		cfg, err := loadConfig(p)
		if err != nil {
			return 0, err
		}
		if err := cfg.applyOverrides(o); err != nil {
			return 0, err
		}
		cfg.applyDefaults(p)
		return cfg.MaxBundleBytes, nil
	}
	if got, _ := load(Overrides{}); got != 1<<20 {
		t.Errorf("config: %d", got)
	}
	t.Setenv("RLY_MAX_BUNDLE_BYTES", "2M")
	if got, _ := load(Overrides{}); got != 2<<20 {
		t.Errorf("env: %d", got)
	}
	if got, _ := load(Overrides{MaxBundleBytes: "3M"}); got != 3<<20 {
		t.Errorf("flag: %d", got)
	}
	if _, err := load(Overrides{MaxBundleBytes: "0"}); err == nil {
		t.Error("0 should be rejected")
	}
	os.WriteFile(p.ConfigFile, []byte("max_bundle_bytes = -5\n"), 0o644)
	if _, err := loadConfig(p); err == nil {
		t.Error("negative config value should be rejected")
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
	if _, err := os.Stat(filepath.Join(root, ".rly", "logs")); err == nil {
		t.Error("init should not create .rly/logs")
	}
	gi, _ := os.ReadFile(filepath.Join(root, ".rly", ".gitignore"))
	if strings.Contains(string(gi), "/logs/") || !strings.Contains(string(gi), "/staging/") {
		t.Errorf(".gitignore = %q", gi)
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
[[logs]]
path = "logs/*.log"
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

	if got := get("logs/server.log"); got != "1\n2\n3\n4\n" {
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

// incrementalEnv is a project with one log, one HTTP target and one
// snapshot, pushing through a fake ssh, with a clock that ticks per call.
type incrementalEnv struct {
	t          *testing.T
	p          *Project
	logFile    string
	body       *string
	remoteHome string
	goodSSH    string
}

func newIncrementalEnv(t *testing.T) *incrementalEnv {
	clearEnv(t)
	body := `{"n":1}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	e := &incrementalEnv{t: t, body: &body, remoteHome: t.TempDir()}
	e.goodSSH = fakeSSH(t, e.remoteHome)
	e.p = newProject(t, fmt.Sprintf(`
[remote]
target = "box"
path = "~/inbox"
[[logs]]
path = "logs/*.log"
[[http]]
name = "health"
url = "%s/h"
[[snapshot]]
name = "db"
command = "cat data.db > {out}"
output = "app.db"
`, srv.URL), "")
	e.setSSH(e.goodSSH)
	e.logFile = filepath.Join(e.p.Root, "logs", "a.log")
	os.MkdirAll(filepath.Dir(e.logFile), 0o755)
	os.WriteFile(filepath.Join(e.p.Root, "data.db"), []byte("db1"), 0o644)
	os.WriteFile(e.logFile, []byte("1\n2\n"), 0o644)
	t.Chdir(e.p.Root)

	clock := time.Date(2026, 10, 15, 12, 0, 0, 0, time.Local)
	old := nowFunc
	nowFunc = func() time.Time { clock = clock.Add(time.Minute); return clock }
	t.Cleanup(func() { nowFunc = old })
	return e
}

func (e *incrementalEnv) setSSH(cmd string) {
	os.WriteFile(e.p.localConfigFile(), []byte(fmt.Sprintf("[remote]\nssh = %q\n", cmd)), 0o644)
}

func (e *incrementalEnv) appendLog(s string) {
	f, err := os.OpenFile(e.logFile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		e.t.Fatal(err)
	}
	f.WriteString(s)
	f.Close()
}

// pushed returns the remote bundles' contents in push order (names sort by
// time), each keyed by path inside the bundle.
func (e *incrementalEnv) pushed() []map[string]string {
	paths, _ := filepath.Glob(filepath.Join(e.remoteHome, "inbox", "*-bundle.tar.gz"))
	sort.Strings(paths)
	var out []map[string]string
	for _, p := range paths {
		files := map[string]string{}
		for k, v := range readTarGz(e.t, p) {
			files[k[strings.Index(k, "/")+1:]] = v
		}
		out = append(out, files)
	}
	return out
}

func (e *incrementalEnv) state() *State {
	st, err := loadState(filepath.Join(e.p.RlyDir, stateName))
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func TestIncrementalBundles(t *testing.T) {
	e := newIncrementalEnv(t)
	bundle := func(args ...string) error { return run(append([]string{"bundle"}, args...)) }

	if err := bundle(); err != nil {
		t.Fatal(err)
	}
	b := e.pushed()
	if len(b) != 1 || b[0]["logs/a.log"] != "1\n2\n" || b[0]["http/health.json"] != `{"n":1}` || b[0]["snapshot/app.db"] != "db1" {
		t.Fatalf("first bundle: %v", b)
	}

	// Nothing changed: no new bundle.
	if err := bundle(); err != nil {
		t.Fatal(err)
	}
	if n := len(e.pushed()); n != 1 {
		t.Fatalf("unchanged run created a bundle (%d total)", n)
	}

	// Appended lines only; the partial trailing line waits.
	e.appendLog("3\n4")
	if err := bundle(); err != nil {
		t.Fatal(err)
	}
	b = e.pushed()
	if len(b) != 2 || b[1]["logs/a.log"] != "3\n" {
		t.Fatalf("delta bundle: %q", b[len(b)-1]["logs/a.log"])
	}
	if _, ok := b[1]["http/health.json"]; ok {
		t.Error("unchanged http response re-sent")
	}
	if _, ok := b[1]["snapshot/app.db"]; ok {
		t.Error("unchanged snapshot re-sent")
	}
	if !strings.Contains(b[1]["MANIFEST.md"], "health.json: unchanged") || !strings.Contains(b[1]["MANIFEST.md"], "previous bundle: 20261015-") {
		t.Errorf("manifest:\n%s", b[1]["MANIFEST.md"])
	}

	// Completing the line, plus changed http and snapshot.
	e.appendLog("\n")
	*e.body = `{"n":2}`
	os.WriteFile(filepath.Join(e.p.Root, "data.db"), []byte("db2"), 0o644)
	if err := bundle(); err != nil {
		t.Fatal(err)
	}
	b = e.pushed()
	if len(b) != 3 || b[2]["logs/a.log"] != "4\n" || b[2]["http/health.json"] != `{"n":2}` || b[2]["snapshot/app.db"] != "db2" {
		t.Fatalf("third bundle: %v", b[2])
	}

	// Truncation resends from the start.
	os.WriteFile(e.logFile, []byte("x\n"), 0o644)
	if err := bundle(); err != nil {
		t.Fatal(err)
	}
	if got := e.pushed()[3]["logs/a.log"]; got != "x\n" {
		t.Fatalf("after truncation: %q", got)
	}

	// Replacement (new inode, larger than the old offset) resends too. The
	// new file is created before the old one goes, so the inode can't be
	// reused; a plain delete+recreate may reuse it and is then only caught
	// if the file is smaller than what was already sent.
	repl := e.logFile + ".new"
	os.WriteFile(repl, []byte("r1\nr2\nr3\n"), 0o644)
	os.Rename(repl, e.logFile)
	if err := bundle(); err != nil {
		t.Fatal(err)
	}
	if got := e.pushed()[4]["logs/a.log"]; got != "r1\nr2\nr3\n" {
		t.Fatalf("after replacement: %q", got)
	}

	// --full sends everything again.
	if err := bundle("--full"); err != nil {
		t.Fatal(err)
	}
	b = e.pushed()
	if len(b) != 6 || b[5]["logs/a.log"] != "r1\nr2\nr3\n" || b[5]["http/health.json"] != `{"n":2}` || b[5]["snapshot/app.db"] != "db2" {
		t.Fatalf("full bundle: %v", b[5])
	}
}

func TestFailedPushStaysPending(t *testing.T) {
	e := newIncrementalEnv(t)
	bundle := func(args ...string) error { return run(append([]string{"bundle"}, args...)) }
	if err := bundle(); err != nil {
		t.Fatal(err)
	}

	failing := filepath.Join(t.TempDir(), "failssh")
	os.WriteFile(failing, []byte("#!/bin/sh\ncat >/dev/null\nexit 1\n"), 0o755)
	e.setSSH(failing)
	e.appendLog("5\n")
	if err := bundle(); err == nil {
		t.Fatal("expected push failure")
	}
	if st := e.state(); len(st.Pending) != 1 || st.Logs[e.logFile].Offset != int64(len("1\n2\n5\n")) {
		t.Fatalf("state should advance with the bundle pending: %+v", st)
	}

	// --no-push adds a second pending bundle without trying to push.
	e.setSSH(e.goodSSH)
	e.appendLog("6\n")
	if err := bundle("--no-push"); err != nil {
		t.Fatal(err)
	}
	if n := len(e.state().Pending); n != 2 {
		t.Fatalf("pending = %d, want 2", n)
	}

	// Nothing new, but pending bundles get pushed, oldest first.
	if err := bundle(); err != nil {
		t.Fatal(err)
	}
	b := e.pushed()
	if len(b) != 3 || b[1]["logs/a.log"] != "5\n" || b[2]["logs/a.log"] != "6\n" {
		t.Fatalf("pending push: %v", b)
	}
	if n := len(e.state().Pending); n != 0 {
		t.Fatalf("pending left: %d", n)
	}

	// A pending bundle plus a new one in the same run; no content repeats.
	e.setSSH(failing)
	e.appendLog("7\n")
	bundle()
	e.setSSH(e.goodSSH)
	e.appendLog("8\n")
	if err := bundle(); err != nil {
		t.Fatal(err)
	}
	b = e.pushed()
	if len(b) != 5 || b[3]["logs/a.log"] != "7\n" || b[4]["logs/a.log"] != "8\n" {
		t.Fatalf("pending+new: %v", b)
	}
}

func TestCopyLogDelta(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.log")
	os.WriteFile(src, []byte("aa\nbbb\ncc\npartial"), 0o644)
	cases := []struct {
		start int64
		want  string
		lines int
	}{
		{0, "aa\nbbb\ncc\n", 3}, // whole file; the partial line waits
		{3, "bbb\ncc\n", 2},
		{10, "", 0}, // only the partial line is left
	}
	for i, c := range cases {
		out := filepath.Join(dir, fmt.Sprintf("out%d", i), "a.log")
		d, err := copyLogDelta(src, out, c.start)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(out)
		if string(got) != c.want || d.lines != c.lines || d.from != c.start || d.end != 10 {
			t.Errorf("case %d: got %q %+v", i, got, d)
		}
	}
	// Large files cross the read-chunk boundary.
	var b strings.Builder
	for i := 0; i < 50000; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	big := filepath.Join(dir, "big.log")
	os.WriteFile(big, []byte(b.String()+"tail"), 0o644)
	d, err := copyLogDelta(big, filepath.Join(dir, "bigout", "big.log"), 0)
	if err != nil || d.lines != 50000 || d.end != int64(b.Len()) {
		t.Fatalf("big: %+v %v", d, err)
	}
}

// fakeTerminal makes the size prompt think stdin is a terminal that answers
// with answer.
func fakeTerminal(t *testing.T, answer string) {
	oldTTY, oldIn := stdinIsTerminal, promptIn
	stdinIsTerminal = func() bool { return true }
	promptIn = strings.NewReader(answer)
	t.Cleanup(func() { stdinIsTerminal, promptIn = oldTTY, oldIn })
}

func noTerminal(t *testing.T) {
	old := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = old })
}

func TestBundleSizeLimit(t *testing.T) {
	e := newIncrementalEnv(t)
	t.Setenv("RLY_MAX_BUNDLE_BYTES", "1K")
	bundle := func(args ...string) error { return run(append([]string{"bundle"}, args...)) }
	lines := func(from, to int) string {
		var b strings.Builder
		for i := from; i <= to; i++ {
			fmt.Fprintf(&b, "line %04d\n", i)
		}
		return b.String()
	}
	staged := func() []string {
		got, _ := filepath.Glob(filepath.Join(e.p.RlyDir, "staging", "*.tar.gz"))
		return got
	}
	statePath := filepath.Join(e.p.RlyDir, stateName)

	// First bundle of a log sends the whole file; at 2000 bytes it is over
	// the 1K limit, so without a terminal it is refused and nothing changes.
	os.WriteFile(e.logFile, []byte(lines(1, 200)), 0o644)
	noTerminal(t)
	err := bundle()
	if err == nil || !strings.Contains(err.Error(), "over max_bundle_bytes 1K") || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("want over-limit error, got %v", err)
	}
	if len(staged()) != 0 || len(e.pushed()) != 0 {
		t.Fatal("over-limit refusal must not write or push a bundle")
	}
	if _, err := os.Stat(statePath); err == nil {
		t.Fatal("over-limit refusal must not write state")
	}

	// --yes sends it, whole.
	if err := bundle("--yes"); err != nil {
		t.Fatal(err)
	}
	if got := e.pushed()[0]["logs/a.log"]; got != lines(1, 200) {
		t.Fatalf("first bundle should carry the whole log, got %d bytes", len(got))
	}
	if err := bundle(); err != nil {
		t.Fatal(err)
	}
	if n := len(e.pushed()); n != 1 {
		t.Fatalf("data sent twice (%d bundles)", n)
	}

	// Over the limit at a terminal: "n" aborts cleanly, state untouched.
	e.appendLog(lines(201, 400))
	before, _ := os.ReadFile(statePath)
	fakeTerminal(t, "n\n")
	if err := bundle(); err != nil {
		t.Fatalf("declining should not be an error: %v", err)
	}
	if after, _ := os.ReadFile(statePath); string(after) != string(before) || len(e.pushed()) != 1 {
		t.Fatal("declining must not bundle or advance state")
	}

	// "y" proceeds, with only the new lines.
	fakeTerminal(t, "y\n")
	if err := bundle(); err != nil {
		t.Fatal(err)
	}
	if got := e.pushed()[1]["logs/a.log"]; got != lines(201, 400) {
		t.Fatalf("after yes: %d bytes", len(got))
	}

	// --full has no limit and never asks, even without a terminal.
	noTerminal(t)
	if err := bundle("--full"); err != nil {
		t.Fatal(err)
	}
	if got := e.pushed()[2]["logs/a.log"]; got != lines(1, 400) {
		t.Fatalf("full should carry the whole log, got %d bytes", len(got))
	}
}

// staged returns the local (unpushed) bundles' contents in order, each keyed
// by path inside the bundle.
func staged(t *testing.T, p *Project) []map[string]string {
	paths, _ := filepath.Glob(filepath.Join(p.RlyDir, "staging", "*-bundle.tar.gz"))
	sort.Strings(paths)
	var out []map[string]string
	for _, path := range paths {
		files := map[string]string{}
		for k, v := range readTarGz(t, path) {
			files[k[strings.Index(k, "/")+1:]] = v
		}
		out = append(out, files)
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLogSources(t *testing.T) {
	clearEnv(t)
	config := func(otherName string) string {
		return fmt.Sprintf(`
[[logs]]
path = "logs/*.log"
skip = ["noisy"]
[[logs]]
path = "other/server.log"
name = %q
[[logs]]
path = "missing/*.log"
`, otherName)
	}
	p := newProject(t, config("other.log"), "")
	writeFile(t, filepath.Join(p.Root, "logs", "server.log"), "s1\n")
	writeFile(t, filepath.Join(p.Root, "logs", "noisy.log"), "n1\n")
	other := filepath.Join(p.Root, "other", "server.log")
	writeFile(t, other, "o1\n")
	t.Chdir(p.Root)

	if err := run([]string{"bundle", "--no-push"}); err != nil {
		t.Fatal(err)
	}
	b := staged(t, p)
	if len(b) != 1 || b[0]["logs/server.log"] != "s1\n" || b[0]["logs/other.log"] != "o1\n" {
		t.Fatalf("first bundle: %v", b)
	}
	if _, ok := b[0]["logs/noisy.log"]; ok {
		t.Error("noisy.log should be skipped")
	}
	if man := b[0]["MANIFEST.md"]; !strings.Contains(man, "nothing matches") || !strings.Contains(man, "missing") {
		t.Errorf("manifest should note the source matching nothing:\n%s", man)
	}

	// State is keyed by source path: renaming a log in the bundle resends
	// nothing, and the next new line lands under the new name.
	os.WriteFile(p.ConfigFile, []byte(config("renamed.log")), 0o644)
	if err := run([]string{"bundle", "--no-push"}); err != nil {
		t.Fatal(err)
	}
	if n := len(staged(t, p)); n != 1 {
		t.Fatalf("renaming should not resend (%d bundles)", n)
	}
	f, _ := os.OpenFile(other, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("o2\n")
	f.Close()
	if err := run([]string{"bundle", "--no-push"}); err != nil {
		t.Fatal(err)
	}
	if b := staged(t, p); len(b) != 2 || b[1]["logs/renamed.log"] != "o2\n" {
		t.Fatalf("after rename: %v", b[len(b)-1])
	}
}

func TestLogSourceNameCollision(t *testing.T) {
	clearEnv(t)
	p := newProject(t, `
[[logs]]
path = "a/server.log"
[[logs]]
path = "b/*.log"
`, "")
	writeFile(t, filepath.Join(p.Root, "a", "server.log"), "a\n")
	writeFile(t, filepath.Join(p.Root, "b", "server.log"), "b\n")
	t.Chdir(p.Root)
	err := run([]string{"bundle", "--no-push"})
	if err == nil || !strings.Contains(err.Error(), "logs/server.log") || !strings.Contains(err.Error(), "name =") {
		t.Fatalf("want collision error, got %v", err)
	}
	if len(staged(t, p)) != 0 {
		t.Fatal("collision must not bundle anything")
	}
	if _, err := os.Stat(filepath.Join(p.RlyDir, stateName)); err == nil {
		t.Fatal("collision must not write state")
	}
}

func TestLogSourceValidation(t *testing.T) {
	clearEnv(t)
	for _, c := range []struct{ config, want string }{
		{"[[logs]]\npath = \"logs/*.log\"\nname = \"x.log\"\n", "is a glob"},
		{"[[logs]]\nname = \"x.log\"\n", "no path"},
		{"[[logs]]\npath = \"a.log\"\nname = \"sub/x.log\"\n", "plain file name"},
		{"[logs]\ndir = \"logs\"\npattern = \"*.log\"\n", "[[logs]]"},
	} {
		p := newProject(t, c.config, "")
		_, err := loadConfig(p)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("config %q: want error containing %q, got %v", c.config, c.want, err)
		}
	}
	// The old table is caught in config.local too.
	p := newProject(t, "", "[logs]\ndir = \"logs\"\n")
	if _, err := loadConfig(p); err == nil || !strings.Contains(err.Error(), "config.local") {
		t.Errorf("old [logs] in config.local: %v", err)
	}
}

func TestLogOverride(t *testing.T) {
	clearEnv(t)
	p := newProject(t, "[[logs]]\npath = \"logs/*.log\"\n", "")
	writeFile(t, filepath.Join(p.Root, "logs", "a.log"), "a\n")
	writeFile(t, filepath.Join(p.Root, "sub", "b.log"), "b\n")
	t.Chdir(filepath.Join(p.Root, "sub"))

	// --log replaces the configured sources, relative to the cwd as typed.
	if err := run([]string{"bundle", "--no-push", "--log", "b.log"}); err != nil {
		t.Fatal(err)
	}
	b := staged(t, p)
	if len(b) != 1 || b[0]["logs/b.log"] != "b\n" {
		t.Fatalf("--log bundle: %v", b)
	}
	if _, ok := b[0]["logs/a.log"]; ok {
		t.Error("--log should replace the configured sources")
	}

	// RLY_LOGS does the same, comma-separated; a flag beats it.
	t.Setenv("RLY_LOGS", "b.log, ../logs/a.log")
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.applyOverrides(Overrides{}); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Logs) != 2 || cfg.Logs[1].Path != filepath.Join(p.Root, "logs", "a.log") {
		t.Fatalf("RLY_LOGS: %+v", cfg.Logs)
	}
	if err := cfg.applyOverrides(Overrides{Logs: []string{"x.log"}}); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Logs) != 1 || filepath.Base(cfg.Logs[0].Path) != "x.log" {
		t.Fatalf("--log over RLY_LOGS: %+v", cfg.Logs)
	}
}
