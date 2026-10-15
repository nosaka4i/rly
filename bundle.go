package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const timestampLayout = "20060102-150405"

// manifest records what went into a bundle and what was skipped, so the
// reader (often an agent) can tell "empty" from "unavailable".
type manifest struct {
	included []string
	skipped  []string
}

func (m *manifest) add(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	m.included = append(m.included, line)
	fmt.Fprintln(os.Stderr, "  +", line)
}

func (m *manifest) skip(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	m.skipped = append(m.skipped, line)
	fmt.Fprintln(os.Stderr, "  -", line)
}

func (m *manifest) write(path, name string, now time.Time) error {
	var b strings.Builder
	host, _ := os.Hostname()
	fmt.Fprintf(&b, "# rly bundle %s\n\n", name)
	fmt.Fprintf(&b, "- host: %s\n- created: %s\n\n", host, now.Format(time.RFC3339))
	b.WriteString("## Included\n\n")
	if len(m.included) == 0 {
		b.WriteString("(nothing)\n")
	}
	for _, l := range m.included {
		fmt.Fprintf(&b, "- %s\n", l)
	}
	b.WriteString("\n## Skipped / unavailable\n\n")
	if len(m.skipped) == 0 {
		b.WriteString("(none)\n")
	}
	for _, l := range m.skipped {
		fmt.Fprintf(&b, "- %s\n", l)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// buildBundle gathers logs, HTTP snapshots and snapshot commands into a
// timestamped .tar.gz in the staging dir and returns its path. Individual
// source failures are recorded in MANIFEST.md, never fatal.
func buildBundle(cfg *Config, now time.Time) (string, error) {
	name := now.Format(timestampLayout) + "-bundle"
	work, err := os.MkdirTemp(cfg.StagingDir, ".rly-tmp-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	root := filepath.Join(work, name)

	m := &manifest{}
	gatherLogs(cfg, filepath.Join(root, "logs"), m)
	gatherHTTP(cfg, filepath.Join(root, "http"), m)
	gatherSnapshots(cfg, filepath.Join(root, "snapshot"), m)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	if err := m.write(filepath.Join(root, "MANIFEST.md"), name, now); err != nil {
		return "", err
	}

	archive := filepath.Join(cfg.StagingDir, name+".tar.gz")
	if err := writeTarGz(archive, work); err != nil {
		return "", err
	}
	return archive, nil
}

func skipMatch(name string, skip []string) bool {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	for _, s := range skip {
		if s == name || s == base {
			return true
		}
		if ok, _ := filepath.Match(s, name); ok {
			return true
		}
	}
	return false
}

func gatherLogs(cfg *Config, dst string, m *manifest) {
	if cfg.Logs.Dir == "" {
		m.skip("logs: no log directory configured")
		return
	}
	matches, err := filepath.Glob(filepath.Join(cfg.Logs.Dir, cfg.Logs.Pattern))
	if err != nil {
		m.skip("logs: bad pattern %q: %v", cfg.Logs.Pattern, err)
		return
	}
	if _, err := os.Stat(cfg.Logs.Dir); err != nil {
		m.skip("logs: %v", err)
		return
	}
	sort.Strings(matches)
	found := 0
	for _, src := range matches {
		base := filepath.Base(src)
		if st, err := os.Stat(src); err != nil || !st.Mode().IsRegular() {
			continue
		}
		if skipMatch(base, cfg.Logs.Skip) {
			m.skip("logs/%s: excluded by skip list", base)
			continue
		}
		found++
		if err := os.MkdirAll(dst, 0o755); err != nil {
			m.skip("logs/%s: %v", base, err)
			continue
		}
		out, err := os.Create(filepath.Join(dst, base))
		if err != nil {
			m.skip("logs/%s: %v", base, err)
			continue
		}
		dropped, err := tailFile(src, out, cfg.Logs.TailLines)
		out.Close()
		switch {
		case err != nil:
			m.skip("logs/%s: %v", base, err)
		case dropped > 0:
			m.add("logs/%s (last %d lines; %d earlier bytes omitted)", base, cfg.Logs.TailLines, dropped)
		default:
			m.add("logs/%s (complete)", base)
		}
	}
	if found == 0 {
		m.skip("logs: no files matching %s in %s", cfg.Logs.Pattern, cfg.Logs.Dir)
	}
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(s string) string {
	s = strings.Trim(unsafeName.ReplaceAllString(s, "_"), "._")
	if s == "" {
		return "unnamed"
	}
	return s
}

func gatherHTTP(cfg *Config, dst string, m *manifest) {
	for _, t := range cfg.HTTP {
		name := safeName(t.Name)
		if t.URL == "" {
			m.skip("http/%s: no url configured", name)
			continue
		}
		timeout, err := parseTimeout(t.Timeout, defaultHTTPTimeout)
		if err != nil {
			m.skip("http/%s: bad timeout %q", name, t.Timeout)
			continue
		}
		client := &http.Client{Timeout: timeout}
		resp, err := client.Get(t.URL)
		if err != nil {
			m.skip("http/%s: GET %s: %v", name, t.URL, err)
			continue
		}
		file := name
		if filepath.Ext(file) == "" {
			file += extForContentType(resp.Header.Get("Content-Type"))
		}
		err = func() error {
			defer resp.Body.Close()
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			out, err := os.Create(filepath.Join(dst, file))
			if err != nil {
				return err
			}
			defer out.Close()
			_, err = io.Copy(out, resp.Body)
			return err
		}()
		switch {
		case err != nil:
			m.skip("http/%s: %v", file, err)
		case resp.StatusCode/100 != 2:
			m.add("http/%s (GET %s returned %s — body saved anyway)", file, t.URL, resp.Status)
		default:
			m.add("http/%s (GET %s)", file, t.URL)
		}
	}
}

func extForContentType(ct string) string {
	mt, _, _ := mime.ParseMediaType(ct)
	switch {
	case mt == "application/json" || strings.HasSuffix(mt, "+json"):
		return ".json"
	case mt == "text/html":
		return ".html"
	default:
		return ".txt"
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func gatherSnapshots(cfg *Config, dst string, m *manifest) {
	for i, s := range cfg.Snapshots {
		label := s.Name
		if label == "" {
			label = fmt.Sprintf("snapshot%d", i+1)
		}
		output := s.Output
		if output == "" {
			output = label
		}
		output = safeName(output)
		if s.Command == "" {
			m.skip("snapshot/%s: no command configured", output)
			continue
		}
		timeout, err := parseTimeout(s.Timeout, defaultSnapTimeout)
		if err != nil {
			m.skip("snapshot/%s: bad timeout %q", output, s.Timeout)
			continue
		}
		if err := os.MkdirAll(dst, 0o755); err != nil {
			m.skip("snapshot/%s: %v", output, err)
			continue
		}
		outPath := filepath.Join(dst, output)
		command := strings.ReplaceAll(s.Command, "{out}", shellQuote(outPath))

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Dir = homeDir()
		if s.Dir != "" {
			cmd.Dir = expandHome(s.Dir)
		}
		cmd.Env = append(os.Environ(), "RLY_OUT="+outPath)
		combined, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			msg := strings.TrimSpace(string(combined))
			if len(msg) > 500 {
				msg = msg[:500] + "…"
			}
			if msg != "" {
				msg = ": " + strings.ReplaceAll(msg, "\n", " ")
			}
			m.skip("snapshot/%s: command failed (%v)%s", output, err, msg)
			os.Remove(outPath)
			continue
		}
		if _, err := os.Stat(outPath); err != nil {
			m.skip("snapshot/%s: command succeeded but did not write {out}", output)
			continue
		}
		m.add("snapshot/%s (from %s)", output, label)
	}
}

// writeTarGz archives the contents of dir into path, atomically.
func writeTarGz(path, dir string) error {
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	err = tw.AddFS(os.DirFS(dir))
	if cerr := tw.Close(); err == nil {
		err = cerr
	}
	if cerr := gz.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
