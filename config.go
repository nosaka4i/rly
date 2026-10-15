package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	rlyDirName        = ".rly"
	configName        = "config"
	localConfigName   = "config.local"
	stagingName       = "staging"
	defaultTailLines  = 10000
	defaultLogPattern = "*.log"
	// The remote inbox defaults to ~/.rly/inbox/<project dir name>.
	defaultRemoteBase  = "~/.rly/inbox"
	defaultHTTPTimeout = 10 * time.Second
	defaultSnapTimeout = 2 * time.Minute
)

// Config is the merged shape of .rly/config and .rly/config.local. Env vars
// and flags only override individual values for one invocation.
type Config struct {
	StagingDir string           `toml:"staging_dir"`
	Remote     RemoteConfig     `toml:"remote"`
	Logs       LogsConfig       `toml:"logs"`
	HTTP       []HTTPTarget     `toml:"http"`
	Snapshots  []SnapshotConfig `toml:"snapshot"`
}

type RemoteConfig struct {
	// Target is anything ssh accepts as a destination: an alias from
	// ~/.ssh/config, host, or user@host.
	Target string `toml:"target"`
	// Path is the directory on the remote side; "~/" is expanded remotely.
	Path string `toml:"path"`
	// SSH is the ssh command to use, e.g. "ssh -p 2222". Defaults to "ssh".
	SSH string `toml:"ssh"`
}

type LogsConfig struct {
	Dir       string   `toml:"dir"`
	Pattern   string   `toml:"pattern"`
	TailLines int      `toml:"tail_lines"`
	Skip      []string `toml:"skip"`
}

type HTTPTarget struct {
	Name    string `toml:"name"`
	URL     string `toml:"url"`
	Timeout string `toml:"timeout"`
}

type SnapshotConfig struct {
	Name    string `toml:"name"`
	Command string `toml:"command"`
	Output  string `toml:"output"`
	Dir     string `toml:"dir"`
	Timeout string `toml:"timeout"`
}

// Overrides holds per-invocation values from flags; empty means "not set".
type Overrides struct {
	StagingDir string
	Remote     string
	RemotePath string
	LogDir     string
	TailLines  string
	Skip       string
}

// Project locates a project directory's .rly (a git repo or any folder).
// Relative paths in its config are resolved against Root, never against the
// working directory.
type Project struct {
	Root       string // directory containing .rly
	RlyDir     string // Root/.rly
	ConfigFile string // RlyDir/config, or an explicit --config/RLY_CONFIG
}

func (p *Project) localConfigFile() string {
	return filepath.Join(filepath.Dir(p.ConfigFile), localConfigName)
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

func expandHome(p string) string {
	if p == "~" {
		return homeDir()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(homeDir(), p[2:])
	}
	return p
}

var errNoProject = errors.New("not in an rly project")

// findProject resolves the config file: flag > RLY_CONFIG > nearest
// .rly/config walking up from the working directory, like git finds .git.
func findProject(flagValue string) (*Project, error) {
	explicit := flagValue
	if explicit == "" {
		explicit = os.Getenv("RLY_CONFIG")
	}
	if explicit != "" {
		cfg, err := filepath.Abs(expandHome(explicit))
		if err != nil {
			return nil, err
		}
		rlyDir := filepath.Dir(cfg)
		return &Project{Root: filepath.Dir(rlyDir), RlyDir: rlyDir, ConfigFile: cfg}, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		cfg := filepath.Join(dir, rlyDirName, configName)
		if st, err := os.Stat(cfg); err == nil && st.Mode().IsRegular() {
			return &Project{Root: dir, RlyDir: filepath.Join(dir, rlyDirName), ConfigFile: cfg}, nil
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return nil, fmt.Errorf("%w: no %s/%s in %s or any parent directory\nrun `rly init` in your project directory first", errNoProject, rlyDirName, configName, cwd)
}

// loadConfig reads .rly/config and overlays .rly/config.local if present:
// tables merge key by key, any other value (including arrays) replaces.
func loadConfig(p *Project) (*Config, error) {
	merged := map[string]any{}
	if _, err := toml.DecodeFile(p.ConfigFile, &merged); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("config file %s not found\nrun `rly init` first", p.ConfigFile)
		}
		return nil, fmt.Errorf("parse %s: %w", p.ConfigFile, err)
	}
	local := map[string]any{}
	if _, err := toml.DecodeFile(p.localConfigFile(), &local); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("parse %s: %w", p.localConfigFile(), err)
	}
	mergeMaps(merged, local)

	var buf strings.Builder
	if err := toml.NewEncoder(&buf).Encode(merged); err != nil {
		return nil, err
	}
	cfg := &Config{}
	if _, err := toml.Decode(buf.String(), cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p.ConfigFile, err)
	}
	return cfg, nil
}

func mergeMaps(dst, src map[string]any) {
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				mergeMaps(dv, sv)
				continue
			}
		}
		dst[k] = v
	}
}

// applyOverrides layers env vars, then flags, over the file config. Paths
// given this way are relative to the working directory, as typed.
func (c *Config) applyOverrides(o Overrides) error {
	set := func(dst *string, env, flag string, isPath bool) {
		v := os.Getenv(env)
		if flag != "" {
			v = flag
		}
		if v == "" {
			return
		}
		if isPath {
			if abs, err := filepath.Abs(expandHome(v)); err == nil {
				v = abs
			}
		}
		*dst = v
	}
	set(&c.StagingDir, "RLY_STAGING", o.StagingDir, true)
	set(&c.Remote.Target, "RLY_REMOTE", o.Remote, false)
	set(&c.Remote.Path, "RLY_REMOTE_PATH", o.RemotePath, false)
	set(&c.Logs.Dir, "RLY_LOG_DIR", o.LogDir, true)

	lines := ""
	set(&lines, "RLY_LOG_TAIL_LINES", o.TailLines, false)
	if lines != "" {
		n, err := strconv.Atoi(lines)
		if err != nil || n <= 0 {
			return fmt.Errorf("invalid tail line count %q", lines)
		}
		c.Logs.TailLines = n
	}

	skip := ""
	set(&skip, "RLY_SKIP_LOGS", o.Skip, false)
	if skip != "" {
		c.Logs.Skip = splitList(skip)
	}
	return nil
}

// resolvePath expands ~ and anchors relative paths at the project root.
func (p *Project) resolvePath(path string) string {
	path = expandHome(path)
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(p.Root, path)
}

func (c *Config) applyDefaults(p *Project) {
	if c.Logs.TailLines <= 0 {
		c.Logs.TailLines = defaultTailLines
	}
	if c.Logs.Pattern == "" {
		c.Logs.Pattern = defaultLogPattern
	}
	if c.Remote.Path == "" {
		c.Remote.Path = defaultRemoteBase
		if p != nil {
			c.Remote.Path += "/" + filepath.Base(p.Root)
		}
	}
	if c.Remote.SSH == "" {
		c.Remote.SSH = "ssh"
	}
	if p == nil {
		return // project-less single-file push: nothing below applies
	}
	if c.StagingDir == "" {
		c.StagingDir = filepath.Join(p.RlyDir, stagingName)
	}
	c.StagingDir = p.resolvePath(c.StagingDir)
	c.Logs.Dir = p.resolvePath(c.Logs.Dir)
	for i := range c.Snapshots {
		if c.Snapshots[i].Dir == "" {
			c.Snapshots[i].Dir = p.Root
		}
		c.Snapshots[i].Dir = p.resolvePath(c.Snapshots[i].Dir)
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseTimeout(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	return time.ParseDuration(s)
}

// findGitRoot returns the nearest ancestor of dir containing .git, or "".
func findGitRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

const configTemplate = `# rly project configuration — check this file in.
# Personal/machine-specific overrides go in .rly/config.local (gitignored),
# which uses the same keys. Env vars (RLY_*) and flags override both per run.
# Relative paths are resolved from the project root (the directory holding .rly).

# Where bundles are written locally (default: .rly/staging, gitignored).
# staging_dir = ".rly/staging"

[remote]
# ssh destination: an alias from ~/.ssh/config, host, or user@host.
# Leave empty to only bundle locally.
target = ""
# Directory on the remote side ("~/" is expanded there).
# Default: ~/.rly/inbox/<project dir name>
# path = "~/.rly/inbox/myproject"
# ssh = "ssh -p 2222"

[logs]
# Directory your processes tee their output into. Every file matching
# 'pattern' is tailed into the bundle.
dir = ""
pattern = "*.log"
tail_lines = 10000
# Names or globs to exclude; "foo" also matches "foo.log".
skip = []

# Live state snapshots: zero or more named HTTP GET targets.
# [[http]]
# name = "health"
# url = "http://localhost:3000/health"
# timeout = "10s"

# Snapshot commands, e.g. a WAL-safe sqlite backup. They run via sh -c from
# the project root (or 'dir'). {out} is replaced by the shell-quoted path the
# command must write to; $RLY_OUT holds it as well.
# [[snapshot]]
# name = "db"
# command = "sqlite3 data/app.db \".backup '{out}'\""
# output = "app.db"
# timeout = "2m"
`

const gitignoreTemplate = `# Managed by rly: bundles and personal overrides stay out of git.
/staging/
/config.local
`

// initProject creates root/.rly with a config template, a .gitignore and
// the staging dir. It never overwrites an existing config.
func initProject(root string) (*Project, error) {
	p := &Project{Root: root, RlyDir: filepath.Join(root, rlyDirName)}
	p.ConfigFile = filepath.Join(p.RlyDir, configName)
	if _, err := os.Stat(p.ConfigFile); err == nil {
		return p, fmt.Errorf("%s already exists; edit it directly", p.ConfigFile)
	}
	if err := os.MkdirAll(filepath.Join(p.RlyDir, stagingName), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p.ConfigFile, []byte(configTemplate), 0o644); err != nil {
		return nil, err
	}
	gi := filepath.Join(p.RlyDir, ".gitignore")
	if _, err := os.Stat(gi); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(gi, []byte(gitignoreTemplate), 0o644); err != nil {
			return nil, err
		}
	}
	return p, nil
}
