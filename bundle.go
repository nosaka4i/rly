package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

const timestampLayout = "20060102-150405"

// manifest records what went into a bundle, what was unchanged since the
// previous one and what was skipped, so the reader (often an agent) can
// tell "nothing new" from "unavailable".
type manifest struct {
	previous  string
	included  []string
	unchanged []string
	skipped   []string
}

func (m *manifest) add(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	m.included = append(m.included, line)
	fmt.Fprintln(os.Stderr, "  +", line)
}

func (m *manifest) same(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	m.unchanged = append(m.unchanged, line)
	fmt.Fprintln(os.Stderr, "  =", line)
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
	fmt.Fprintf(&b, "- host: %s\n- created: %s\n- previous bundle: %s\n\n", host, now.Format(time.RFC3339), m.previous)
	section := func(title string, lines []string, empty string) {
		fmt.Fprintf(&b, "## %s\n\n", title)
		if len(lines) == 0 {
			b.WriteString(empty + "\n")
		}
		for _, l := range lines {
			fmt.Fprintf(&b, "- %s\n", l)
		}
		b.WriteString("\n")
	}
	section("Included (new since previous bundle)", m.included, "(nothing)")
	section("Unchanged since previous bundle (not included)", m.unchanged, "(none)")
	section("Skipped / unavailable", m.skipped, "(none)")
	return os.WriteFile(path, []byte(strings.TrimRight(b.String(), "\n")+"\n"), 0o644)
}

// bundleName picks a timestamped name that doesn't collide with an
// existing archive or a pending one.
func bundleName(cfg *Config, st *State, now time.Time) string {
	for {
		name := now.Format(timestampLayout) + "-bundle"
		archive := name + ".tar.gz"
		_, err := os.Stat(filepath.Join(cfg.StagingDir, archive))
		if errors.Is(err, fs.ErrNotExist) && !slices.Contains(st.Pending, archive) {
			return name
		}
		now = now.Add(time.Second)
	}
}

// errAborted means the user declined an over-limit bundle at the prompt.
var errAborted = errors.New("aborted; nothing bundled")

// sizeCheck decides whether a bundle of total uncompressed bytes may go
// ahead when it exceeds max_bundle_bytes: nil to proceed, errAborted or
// another error to stop with nothing bundled and no state change.
type sizeCheck func(total int64) error

// buildBundle gathers whatever is new since the bundle recorded in st (or
// everything, with full) into a timestamped .tar.gz in the staging dir.
// It returns the archive path ("" if nothing changed) and the state to save
// once the archive exists. Individual source failures are recorded in
// MANIFEST.md, never fatal. Before compressing, a bundle over
// cfg.MaxBundleBytes (uncompressed) is passed to overLimit, unless full.
func buildBundle(cfg *Config, st *State, full bool, now time.Time, overLimit sizeCheck) (string, *State, error) {
	m := &manifest{previous: st.LastBundle}
	switch {
	case full:
		m.previous = "none (full bundle requested)"
	case m.previous == "":
		m.previous = "none"
	}
	logs, err := resolveLogs(cfg.Logs, m)
	if err != nil {
		return "", nil, err
	}

	ns := st.next()
	name := bundleName(cfg, st, now)
	work, err := os.MkdirTemp(cfg.StagingDir, ".rly-tmp-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(work)
	root := filepath.Join(work, name)

	changed := gatherLogs(logs, st, ns, full, filepath.Join(root, "logs"), m)
	changed = gatherHTTP(cfg, st, ns, full, filepath.Join(root, "http"), m) || changed
	changed = gatherSnapshots(cfg, st, ns, full, filepath.Join(root, "snapshot"), m) || changed
	if !changed {
		return "", ns, nil
	}
	for _, d := range []string{"logs", "http", "snapshot"} {
		os.Remove(filepath.Join(root, d)) // only succeeds if empty
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", nil, err
	}
	if !full && cfg.MaxBundleBytes > 0 {
		total, err := dirSize(root)
		if err != nil {
			return "", nil, err
		}
		if total > int64(cfg.MaxBundleBytes) {
			if err := overLimit(total); err != nil {
				return "", nil, err
			}
		}
	}
	if err := m.write(filepath.Join(root, "MANIFEST.md"), name, now); err != nil {
		return "", nil, err
	}

	archive := filepath.Join(cfg.StagingDir, name+".tar.gz")
	if err := writeTarGz(archive, work); err != nil {
		return "", nil, err
	}
	ns.LastBundle = name
	return archive, ns, nil
}

// dirSize sums the sizes of the regular files under dir.
func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		fi, err := d.Info()
		if err == nil {
			total += fi.Size()
		}
		return err
	})
	return total, err
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

// logFile is one log to bundle: its absolute source path and its name
// inside the bundle (logs/<name>).
type logFile struct {
	src, name string
}

// resolveLogs expands the [[logs]] sources into files. A source matching
// nothing is noted in m; two files that would share a bundle name are an
// error, before anything is gathered.
func resolveLogs(sources []LogSource, m *manifest) ([]logFile, error) {
	if len(sources) == 0 {
		m.skip("logs: no log sources configured")
		return nil, nil
	}
	var out []logFile
	owner := map[string]string{} // bundle name -> source path
	for _, s := range sources {
		matches := []string{s.Path}
		if hasGlobMeta(s.Path) {
			var err error
			if matches, err = filepath.Glob(s.Path); err != nil {
				m.skip("logs: bad glob %q: %v", s.Path, err)
				continue
			}
			sort.Strings(matches)
		}
		found := 0
		for _, src := range matches {
			if fi, err := os.Stat(src); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			found++
			name := s.Name
			if name == "" {
				name = filepath.Base(src)
			}
			if skipMatch(filepath.Base(src), s.Skip) {
				m.skip("logs/%s: excluded by skip list", name)
				continue
			}
			if prev, ok := owner[name]; ok {
				if prev == src {
					continue // listed by two sources; bundle it once
				}
				return nil, fmt.Errorf("log sources %s and %s would both be bundled as logs/%s\nset name = \"...\" on one of them in [[logs]]", prev, src, name)
			}
			owner[name] = src
			out = append(out, logFile{src: src, name: name})
		}
		if found == 0 {
			m.skip("logs: nothing matches %s", s.Path)
		}
	}
	return out, nil
}

// gatherLogs copies each log's new complete lines since the offset in st.
// A log seen for the first time, a replaced or truncated log, and any log in
// a --full bundle is read from the start. There are no line caps; the only
// size guard is max_bundle_bytes, checked for the bundle as a whole.
func gatherLogs(logs []logFile, st, ns *State, full bool, dst string, m *manifest) (changed bool) {
	defer pruneLogState(ns)
	for _, lf := range logs {
		src, base := lf.src, lf.name
		fi, err := os.Stat(src)
		if err != nil || !fi.Mode().IsRegular() {
			m.skip("logs/%s: %s disappeared", base, src)
			continue
		}
		dev, ino, hasID := fileID(fi)
		prev := st.Logs[src]
		start, why := int64(0), ""
		switch {
		case full:
			why = "full bundle"
		case prev == nil:
			why = "first bundle of this file"
		case prev.HasID && hasID && (prev.Dev != dev || prev.Ino != ino):
			why = "file was replaced; resent from start"
		case fi.Size() < prev.Offset:
			why = "file was truncated; resent from start"
		default:
			start = prev.Offset
		}
		d, err := copyLogDelta(src, filepath.Join(dst, base), start)
		if err != nil {
			m.skip("logs/%s: %v", base, err)
			continue
		}
		ns.Logs[src] = &logState{Offset: d.end, Dev: dev, Ino: ino, HasID: hasID}
		if d.lines == 0 {
			m.same("logs/%s: no new complete lines", base)
			continue
		}
		changed = true
		note := ""
		if why != "" {
			note = "; " + why
		}
		m.add("logs/%s: +%d lines (bytes %d–%d%s)", base, d.lines, d.from, d.end, note)
	}
	return changed
}

// pruneLogState forgets logs that no longer exist.
func pruneLogState(ns *State) {
	for path := range ns.Logs {
		if _, err := os.Stat(path); err != nil {
			delete(ns.Logs, path)
		}
	}
}

const readChunk = 64 * 1024

type logDelta struct {
	from, end int64 // byte range written
	lines     int   // complete lines written
}

// copyLogDelta writes the complete lines of src in [start, EOF) to dst. A
// trailing partial line is left for the next bundle. The size is sampled
// once, so lines tee appends meanwhile simply wait for the next bundle. dst
// is only created when there is something to write.
func copyLogDelta(src, dst string, start int64) (logDelta, error) {
	f, err := os.Open(src)
	if err != nil {
		return logDelta{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return logDelta{}, err
	}
	end, err := lastLineEnd(f, start, fi.Size())
	if err != nil {
		return logDelta{}, err
	}
	d := logDelta{from: start, end: end}
	if end == start {
		return d, nil
	}
	if d.lines, err = countLines(io.NewSectionReader(f, d.from, d.end-d.from)); err != nil {
		return d, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return d, err
	}
	out, err := os.Create(dst)
	if err != nil {
		return d, err
	}
	_, err = io.Copy(out, io.NewSectionReader(f, d.from, d.end-d.from))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return d, err
}

// lastLineEnd returns the offset just past the last '\n' in [start, size),
// or start if there is none.
func lastLineEnd(r io.ReaderAt, start, size int64) (int64, error) {
	buf := make([]byte, readChunk)
	for pos := size; pos > start; {
		n := min(int64(len(buf)), pos-start)
		pos -= n
		if _, err := r.ReadAt(buf[:n], pos); err != nil && err != io.EOF {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return pos + int64(i) + 1, nil
		}
	}
	return start, nil
}

func countLines(r io.Reader) (int, error) {
	buf := make([]byte, readChunk)
	n := 0
	for {
		k, err := r.Read(buf)
		n += bytes.Count(buf[:k], []byte{'\n'})
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// keepIfChanged records path's hash under key and reports whether it
// differs from the previous bundle; unchanged files are removed.
func keepIfChanged(path, key string, prev, next map[string]string, full bool) (bool, error) {
	h, err := hashFile(path)
	if err != nil {
		return false, err
	}
	next[key] = h
	if !full && prev[key] == h {
		return false, os.Remove(path)
	}
	return true, nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(s string) string {
	s = strings.Trim(unsafeName.ReplaceAllString(s, "_"), "._")
	if s == "" {
		return "unnamed"
	}
	return s
}

func gatherHTTP(cfg *Config, st, ns *State, full bool, dst string, m *manifest) (changed bool) {
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
		if err != nil {
			m.skip("http/%s: %v", file, err)
			continue
		}
		fresh, err := keepIfChanged(filepath.Join(dst, file), file, st.HTTP, ns.HTTP, full)
		switch {
		case err != nil:
			m.skip("http/%s: %v", file, err)
		case !fresh:
			m.same("http/%s: unchanged", file)
		case resp.StatusCode/100 != 2:
			changed = true
			m.add("http/%s (GET %s returned %s — body saved anyway)", file, t.URL, resp.Status)
		default:
			changed = true
			m.add("http/%s (GET %s)", file, t.URL)
		}
	}
	return changed
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

func gatherSnapshots(cfg *Config, st, ns *State, full bool, dst string, m *manifest) (changed bool) {
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
		cmd.Dir = s.Dir
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
		fresh, err := keepIfChanged(outPath, output, st.Snapshots, ns.Snapshots, full)
		switch {
		case err != nil:
			m.skip("snapshot/%s: %v", output, err)
		case !fresh:
			m.same("snapshot/%s: unchanged", output)
		default:
			changed = true
			m.add("snapshot/%s (from %s)", output, label)
		}
	}
	return changed
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
