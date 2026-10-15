package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	defaultTailLines   = 10000
	defaultLogPattern  = "*.log"
	defaultRemotePath  = "~/.rly/inbox"
	defaultHTTPTimeout = 10 * time.Second
	defaultSnapTimeout = 2 * time.Minute
)

// Config is the on-disk shape of ~/.rly/config. It is the source of truth;
// env vars and flags only override individual values for one invocation.
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

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

// configPath resolves which config file to use: flag > RLY_CONFIG > ~/.rly/config.
func configPath(flagValue string) string {
	if flagValue != "" {
		return expandHome(flagValue)
	}
	if v := os.Getenv("RLY_CONFIG"); v != "" {
		return expandHome(v)
	}
	return filepath.Join(homeDir(), ".rly", "config")
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

// loadConfig reads the config file. A missing file yields an empty config;
// callers decide which missing values are fatal.
func loadConfig(path string) (*Config, bool, error) {
	cfg := &Config{}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if _, err := toml.Decode(string(data), cfg); err != nil {
		return nil, true, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, true, nil
}

// applyOverrides layers env vars, then flags, over the file config.
func (c *Config) applyOverrides(o Overrides) error {
	set := func(dst *string, env, flag string) {
		if v := os.Getenv(env); v != "" {
			*dst = v
		}
		if flag != "" {
			*dst = flag
		}
	}
	set(&c.StagingDir, "RLY_HOME", o.StagingDir)
	set(&c.Remote.Target, "RLY_REMOTE", o.Remote)
	set(&c.Remote.Path, "RLY_REMOTE_PATH", o.RemotePath)
	set(&c.Logs.Dir, "RLY_LOG_DIR", o.LogDir)

	lines := ""
	set(&lines, "RLY_LOG_TAIL_LINES", o.TailLines)
	if lines != "" {
		n, err := strconv.Atoi(lines)
		if err != nil || n <= 0 {
			return fmt.Errorf("invalid tail line count %q", lines)
		}
		c.Logs.TailLines = n
	}

	skip := ""
	set(&skip, "RLY_SKIP_LOGS", o.Skip)
	if skip != "" {
		c.Logs.Skip = splitList(skip)
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.Logs.TailLines <= 0 {
		c.Logs.TailLines = defaultTailLines
	}
	if c.Logs.Pattern == "" {
		c.Logs.Pattern = defaultLogPattern
	}
	if c.Remote.Path == "" {
		c.Remote.Path = defaultRemotePath
	}
	if c.Remote.SSH == "" {
		c.Remote.SSH = "ssh"
	}
	c.StagingDir = expandHome(c.StagingDir)
	c.Logs.Dir = expandHome(c.Logs.Dir)
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

// requireStagingDir enforces the "never silently fall back to cwd" rule.
func (c *Config) requireStagingDir(cfgPath string) error {
	if c.StagingDir == "" {
		return fmt.Errorf("no staging directory configured (looked in %s and $RLY_HOME)\nrun `rly init [DIR]` first", cfgPath)
	}
	st, err := os.Stat(c.StagingDir)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("staging directory %s does not exist\nrun `rly init %s` to create it", c.StagingDir, c.StagingDir)
	}
	return nil
}

func parseTimeout(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	return time.ParseDuration(s)
}

const configTemplate = `# rly configuration. Env vars (RLY_*) and flags override values here per run.

# Local directory where bundles are written. Set by ` + "`rly init`" + `.
staging_dir = %s

[remote]
# ssh destination: an alias from ~/.ssh/config, host, or user@host.
# Leave empty to only bundle locally.
target = ""
# Directory on the remote side ("~/" is expanded there).
path = "~/.rly/inbox"
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

# Snapshot commands, e.g. a WAL-safe sqlite backup. {out} is replaced by the
# (shell-quoted) path the command must write to; $RLY_OUT holds it as well.
# [[snapshot]]
# name = "db"
# command = "sqlite3 ~/proj/app.db \".backup '{out}'\""
# output = "app.db"
# timeout = "2m"
`

var stagingLineRe = regexp.MustCompile(`(?m)^[ \t]*staging_dir[ \t]*=.*$`)

// writeStagingDir persists dir into the config file, creating it from the
// template if needed and otherwise editing only the staging_dir line so
// user comments survive.
func writeStagingDir(cfgPath, dir string) error {
	quoted := strconv.Quote(dir)
	data, err := os.ReadFile(cfgPath)
	var out string
	switch {
	case errors.Is(err, fs.ErrNotExist):
		out = fmt.Sprintf(configTemplate, quoted)
	case err != nil:
		return err
	case stagingLineRe.Match(data):
		out = stagingLineRe.ReplaceAllLiteralString(string(data), "staging_dir = "+quoted)
	default:
		// Top-level keys must precede any [table], so prepend.
		out = "staging_dir = " + quoted + "\n" + string(data)
	}
	var check Config
	if _, err := toml.Decode(out, &check); err != nil {
		return fmt.Errorf("refusing to write invalid config %s: %w", cfgPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(cfgPath, []byte(out), 0o644)
}
