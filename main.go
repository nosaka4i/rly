// Command rly bundles logs and live state on one machine and relays them,
// over existing ssh access, to another (typically where an AI coding agent
// is working).
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
)

var version = "0.0.1"

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
	full       bool
	yes        bool
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
	f.StringArrayVar(&opts.o.Logs, "log", nil, "log file or glob to bundle instead of the configured [[logs]]; repeatable (env RLY_LOGS, comma-separated)")
	f.StringVar(&opts.o.MaxBundleBytes, "max-bundle-bytes", "", `ask before bundles larger than this, e.g. "200M" (env RLY_MAX_BUNDLE_BYTES; default 50M)`)
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
		Short: "Bundle what's new since the last bundle and push it",
		Long: `Gather what is new since the previous bundle — appended log lines, and
HTTP responses or snapshot outputs whose content changed — into one
timestamped archive in the staging dir, then push it. Nothing new means no
bundle. Progress is tracked in .rly/state.json; bundles whose push failed
or was skipped stay pending and are pushed (oldest first) on the next run,
so each piece of data is sent exactly once. --full sends everything.

A bundle larger than max_bundle_bytes (uncompressed; default 50M) needs
confirmation: rly asks when run from a terminal and refuses otherwise,
unless --yes is given. --full bundles are never limited.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runBundle(opts) },
	}
	opts.addBundleFlags(cmd)
	cmd.Flags().BoolVar(&opts.noPush, "no-push", false, "build the bundle locally only (it stays pending for the next push)")
	cmd.Flags().BoolVar(&opts.full, "full", false, "include everything from the start of each log, with no size limit")
	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false, "send a bundle over max_bundle_bytes without asking")
	return cmd
}

// nowFunc is swapped in tests to give bundles distinct timestamps.
var nowFunc = time.Now

// stdinIsTerminal and promptIn are swapped in tests to drive the size
// prompt without a real terminal.
var (
	stdinIsTerminal = func() bool {
		fi, err := os.Stdin.Stat()
		return err == nil && fi.Mode()&os.ModeCharDevice != 0
	}
	promptIn io.Reader = os.Stdin
)

// confirmSize returns the over-limit policy for a bundle: --yes proceeds,
// a terminal gets asked, anything else is refused with a hint.
func confirmSize(cfg *Config, yes bool) sizeCheck {
	return func(total int64) error {
		if yes {
			return nil
		}
		desc := fmt.Sprintf("bundle would be %s (~%s compressed), over max_bundle_bytes %s",
			formatSize(total), formatSize(total/10), formatSize(int64(cfg.MaxBundleBytes)))
		if !stdinIsTerminal() {
			return fmt.Errorf("%s; nothing bundled\nrerun with --yes to send it anyway, raise --max-bundle-bytes, or use --full", desc)
		}
		fmt.Fprintf(os.Stderr, "rly: %s; continue? [y/N] ", desc)
		answer, _ := bufio.NewReader(promptIn).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
			return nil
		}
		return errAborted
	}
}

// runBundle pushes any bundles left pending by earlier failed or skipped
// pushes, then builds and pushes a bundle of what is new since the last
// one. State advances as soon as an archive is written, so every byte of
// data lands in exactly one bundle; unpushed archives stay pending.
func runBundle(opts *options) error {
	cfg, p, err := opts.resolve()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StagingDir, 0o755); err != nil {
		return err
	}
	statePath := filepath.Join(p.RlyDir, stateName)
	st, err := loadState(statePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rly: warning: %v; treating this as the first bundle\n", err)
	}

	var pushErr error
	if !opts.noPush && cfg.Remote.Target != "" {
		pushErr = pushPending(cfg, st, statePath)
	}

	fmt.Fprintln(os.Stderr, "rly: bundling")
	archive, ns, err := buildBundle(cfg, st, opts.full, nowFunc(), confirmSize(cfg, opts.yes))
	if errors.Is(err, errAborted) {
		fmt.Fprintln(os.Stderr, "rly:", err)
		return pushErr
	}
	if err != nil {
		return err
	}
	if archive == "" {
		if err := ns.save(statePath); err != nil {
			return fmt.Errorf("save state: %w", err)
		}
		last := st.LastBundle
		if last == "" {
			last = "(no previous bundle)"
		}
		fmt.Fprintln(os.Stderr, "rly: nothing new since", last)
		return pushErr
	}
	name := filepath.Base(archive)
	ns.Pending = append(ns.Pending, name)
	if err := ns.save(statePath); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	fmt.Fprintln(os.Stderr, "rly: wrote", archive)

	switch {
	case pushErr != nil:
		return fmt.Errorf("%w; %d bundle(s) pending, will retry next run", pushErr, len(ns.Pending))
	case opts.noPush:
		fmt.Fprintf(os.Stderr, "rly: not pushed (--no-push); %d bundle(s) pending for the next push\n", len(ns.Pending))
		return nil
	case cfg.Remote.Target == "":
		fmt.Fprintf(os.Stderr, "rly: no remote target configured; %d bundle(s) pending locally\n", len(ns.Pending))
		return nil
	}
	if err := pushFile(cfg, archive, name); err != nil {
		return fmt.Errorf("%w (bundle kept at %s; pending, will retry next run)", err, archive)
	}
	ns.Pending = slices.DeleteFunc(ns.Pending, func(n string) bool { return n == name })
	if err := ns.save(statePath); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	fmt.Fprintln(os.Stderr, "rly: pushed to", remoteDisplay(cfg, name))
	return nil
}

// pushPending pushes earlier unpushed bundles oldest first, stopping at the
// first failure so order is preserved. State is saved after each push.
func pushPending(cfg *Config, st *State, statePath string) error {
	for len(st.Pending) > 0 {
		name := st.Pending[0]
		archive := filepath.Join(cfg.StagingDir, name)
		if _, err := os.Stat(archive); err != nil {
			fmt.Fprintf(os.Stderr, "rly: warning: pending bundle %s is missing from %s; dropping it\n", name, cfg.StagingDir)
		} else if err := pushFile(cfg, archive, name); err != nil {
			return fmt.Errorf("pending bundle %s: %w", name, err)
		} else {
			fmt.Fprintln(os.Stderr, "rly: pushed pending", remoteDisplay(cfg, name))
		}
		st.Pending = st.Pending[1:]
		if err := st.save(statePath); err != nil {
			return fmt.Errorf("save state: %w", err)
		}
	}
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
			name := nowFunc().Format(timestampLayout) + "-" + filepath.Base(local)
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
