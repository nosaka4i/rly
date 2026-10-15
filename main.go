// Command rly bundles logs and live state on one machine and relays them,
// over existing ssh access, to another (typically where an AI coding agent
// is working).
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "rly:", err)
		os.Exit(1)
	}
}

// run builds a fresh command tree per call so flag state never leaks
// between invocations (tests call it repeatedly).
func run(args []string) error {
	root := newRootCmd()
	root.SetArgs(args)
	return root.Execute()
}

// options is the per-invocation flag state shared by all subcommands.
type options struct {
	configFile string
	o          Overrides
	noPush     bool
}

func (opts *options) addRemoteFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&opts.o.Remote, "remote", "", "ssh destination: alias, host, or user@host (env RLY_REMOTE)")
	f.StringVar(&opts.o.RemotePath, "remote-path", "", "remote directory (env RLY_REMOTE_PATH)")
}

func (opts *options) addBundleFlags(cmd *cobra.Command) {
	opts.addRemoteFlags(cmd)
	f := cmd.Flags()
	f.StringVar(&opts.o.StagingDir, "staging", "", "local staging directory (env RLY_STAGING)")
	f.StringVar(&opts.o.LogDir, "log-dir", "", "directory of tee'd logs (env RLY_LOG_DIR)")
	f.StringVar(&opts.o.TailLines, "lines", "", "lines to keep per log (env RLY_LOG_TAIL_LINES)")
	f.StringVar(&opts.o.Skip, "skip", "", "comma-separated log names/globs to exclude (env RLY_SKIP_LOGS)")
}

func (opts *options) resolve() (*Config, *Project, error) {
	p, err := findProject(opts.configFile)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := loadConfig(p)
	if err != nil {
		return nil, p, err
	}
	if err := cfg.applyOverrides(opts.o); err != nil {
		return nil, p, err
	}
	cfg.applyDefaults(p)
	return cfg, p, nil
}

// resolveForPush is resolve, except that a missing project is fine: a
// single-file push only needs a remote, which flags or env can supply.
func (opts *options) resolveForPush() (*Config, error) {
	cfg, _, err := opts.resolve()
	if err == nil || !errors.Is(err, errNoProject) {
		return cfg, err
	}
	cfg = &Config{}
	if err := cfg.applyOverrides(opts.o); err != nil {
		return nil, err
	}
	cfg.applyDefaults(nil)
	if cfg.Remote.Target == "" {
		return nil, fmt.Errorf("%w\n(or pass --remote / set RLY_REMOTE to push without a project)", err)
	}
	return cfg, nil
}

func newRootCmd() *cobra.Command {
	opts := &options{}
	root := &cobra.Command{
		Use:   "rly",
		Short: "Relay log bundles to another machine",
		Long: `rly snapshots logs and live state (HTTP endpoints, DB backups) into one
timestamped archive and pushes it over existing ssh access.

Each project directory (a git repo or any other folder) keeps its own
.rly/ directory: .rly/config (checked in),
.rly/config.local (personal overrides, gitignored) and .rly/staging/
(bundles, gitignored). rly finds it by walking up from the current
directory, like git finds .git; create it with "rly init".

Precedence: flag > env var > .rly/config.local > .rly/config.`,
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
		// Keep `rly completion` working but out of the usage listing.
		CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: true},
	}
	root.PersistentFlags().StringVar(&opts.configFile, "config", "", "config file (env RLY_CONFIG; default: nearest .rly/config)")

	root.AddCommand(
		newBundleCmd(opts),
		newPushCmd(opts),
		newInitCmd(opts),
		newConfigCmd(opts),
		&cobra.Command{
			Use:   "version",
			Short: "Print the rly version",
			Args:  cobra.NoArgs,
			Run:   func(cmd *cobra.Command, _ []string) { fmt.Println("rly", version) },
		},
	)
	return root
}

func newBundleCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bundle",
		Short: "Gather logs and snapshots into an archive and push it",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runBundle(opts) },
	}
	opts.addBundleFlags(cmd)
	cmd.Flags().BoolVar(&opts.noPush, "no-push", false, "build the bundle locally only")
	return cmd
}

func runBundle(opts *options) error {
	cfg, _, err := opts.resolve()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StagingDir, 0o755); err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "rly: bundling")
	archive, err := buildBundle(cfg, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "rly: wrote", archive)

	if opts.noPush {
		return nil
	}
	if cfg.Remote.Target == "" {
		fmt.Fprintln(os.Stderr, "rly: no remote target configured; bundle kept locally only")
		return nil
	}
	name := filepath.Base(archive)
	if err := pushFile(cfg, archive, name); err != nil {
		return fmt.Errorf("%w (bundle kept at %s)", err, archive)
	}
	fmt.Fprintln(os.Stderr, "rly: pushed to", remoteDisplay(cfg, name))
	return nil
}

func newPushCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "push FILE",
		Short: "Push one file as-is, timestamp-prefixed",
		Long: `Push a single file to the remote directory as <timestamp>-<name>.

Skips all log/snapshot gathering and the staging directory: only this one
file is transferred. Works outside any project when the remote is given
via --remote or RLY_REMOTE.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			local := args[0]
			st, err := os.Stat(local)
			if err != nil {
				return err
			}
			if !st.Mode().IsRegular() {
				return fmt.Errorf("%s is not a regular file", local)
			}
			cfg, err := opts.resolveForPush()
			if err != nil {
				return err
			}
			name := time.Now().Format(timestampLayout) + "-" + filepath.Base(local)
			if err := pushFile(cfg, local, name); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "rly: pushed to", remoteDisplay(cfg, name))
			return nil
		},
	}
	opts.addRemoteFlags(cmd)
	return cmd
}

func newInitCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "init [DIR]",
		Short: "Create .rly/ (config, .gitignore, staging) for a project",
		Long: `Create DIR/.rly/ with a commented config template, a .gitignore for the
staging dir and config.local, and the staging dir itself.

DIR defaults to the root of the enclosing git repo if there is one,
otherwise the current directory. Git is not required. An existing .rly/config is never overwritten.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := ""
			if len(args) == 1 {
				root = expandHome(args[0])
			} else {
				cwd, err := os.Getwd()
				if err != nil {
					return err
				}
				root = findGitRoot(cwd)
				if root == "" {
					root = cwd
				}
			}
			root, err := filepath.Abs(root)
			if err != nil {
				return err
			}
			p, err := initProject(root)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "rly: created %s\nrly: edit %s, then check it in\n", p.RlyDir, p.ConfigFile)
			return nil
		},
	}
}

func newConfigCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Print the resolved configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, p, err := opts.resolve()
			if err != nil {
				return err
			}
			fmt.Printf("# project root: %s\n# config file:  %s\n", p.Root, p.ConfigFile)
			if _, err := os.Stat(p.localConfigFile()); err == nil {
				fmt.Printf("# local config: %s\n", p.localConfigFile())
			}
			return toml.NewEncoder(os.Stdout).Encode(cfg)
		},
	}
	opts.addBundleFlags(cmd)
	return cmd
}
