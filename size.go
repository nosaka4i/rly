package main

import (
	"fmt"
	"strconv"
	"strings"
)

// ByteSize is a byte count that config accepts as a TOML integer or as a
// size string like "50M", "1.5G" or "512KiB" (base 1024).
type ByteSize int64

// UnmarshalTOML implements toml.Unmarshaler.
func (b *ByteSize) UnmarshalTOML(v any) error {
	var n int64
	switch v := v.(type) {
	case int64:
		n = v
	case string:
		var err error
		if n, err = parseSize(v); err != nil {
			return err
		}
	default:
		return fmt.Errorf("invalid size %v: want an integer or a string like \"50M\"", v)
	}
	if n <= 0 {
		return fmt.Errorf("invalid size %v: must be positive", v)
	}
	*b = ByteSize(n)
	return nil
}

// MarshalText renders sizes readably in `rly config` output.
func (b ByteSize) MarshalText() ([]byte, error) {
	return []byte(formatSize(int64(b))), nil
}

// parseSize parses "52428800", "50M", "50MB", "50MiB", "1.5g", "512k"
// (case-insensitive, base 1024). The result must be positive.
func parseSize(s string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	t = strings.TrimSuffix(t, "ib")
	t = strings.TrimSuffix(t, "b")
	mult := 1.0
	if t != "" {
		switch t[len(t)-1] {
		case 'k':
			mult = 1 << 10
		case 'm':
			mult = 1 << 20
		case 'g':
			mult = 1 << 30
		case 't':
			mult = 1 << 40
		}
		if mult != 1 {
			t = strings.TrimSpace(t[:len(t)-1])
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || f <= 0 || (mult == 1 && f != float64(int64(f))) {
		return 0, fmt.Errorf("invalid size %q: want a positive byte count or a size like \"50M\"", s)
	}
	return int64(f * mult), nil
}

// formatSize renders n in the largest whole unit that keeps one decimal,
// e.g. 52428800 -> "50M", 1610612736 -> "1.5G".
func formatSize(n int64) string {
	units := []struct {
		suffix string
		size   int64
	}{{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}}
	for _, u := range units {
		if n >= u.size {
			s := strconv.FormatFloat(float64(n)/float64(u.size), 'f', 1, 64)
			return strings.TrimSuffix(s, ".0") + u.suffix
		}
	}
	return strconv.FormatInt(n, 10) + "B"
}
