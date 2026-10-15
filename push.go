package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// remoteDirExpr renders a remote directory for the remote shell, letting a
// leading "~" expand there while quoting everything else.
func remoteDirExpr(p string) string {
	switch {
	case p == "~":
		return `"$HOME"`
	case strings.HasPrefix(p, "~/"):
		return `"$HOME"/` + shellQuote(p[2:])
	default:
		return shellQuote(p)
	}
}

// pushFile streams one local file to <remote.path>/<remoteName> over a
// single ssh connection. It needs nothing on the remote side beyond a POSIX
// shell, and writes to a temp name first so readers never see partial files.
func pushFile(cfg *Config, local, remoteName string) error {
	if cfg.Remote.Target == "" {
		return errors.New("no remote target configured (set [remote] target in config, $RLY_REMOTE, or --remote)")
	}
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()

	dir := remoteDirExpr(strings.TrimRight(cfg.Remote.Path, "/"))
	final := dir + "/" + shellQuote(remoteName)
	part := dir + "/" + shellQuote("."+remoteName+".part")
	script := fmt.Sprintf("mkdir -p %s && cat > %s && mv -f %s %s", dir, part, part, final)

	sshArgs := strings.Fields(cfg.Remote.SSH)
	if len(sshArgs) == 0 {
		sshArgs = []string{"ssh"}
	}
	args := append(sshArgs[1:], cfg.Remote.Target, script)
	cmd := exec.Command(sshArgs[0], args...)
	cmd.Stdin = f
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("push to %s failed: %w", cfg.Remote.Target, err)
	}
	return nil
}

func remoteDisplay(cfg *Config, name string) string {
	return cfg.Remote.Target + ":" + strings.TrimRight(cfg.Remote.Path, "/") + "/" + name
}
