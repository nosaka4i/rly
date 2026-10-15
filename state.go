package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

const stateName = "state.json"

// State remembers what earlier bundles already contained, so each bundle
// carries only new data, plus which bundles still await a push.
type State struct {
	LastBundle string               `json:"last_bundle,omitempty"`
	Pending    []string             `json:"pending,omitempty"` // archive names in staging, oldest first
	Logs       map[string]*logState `json:"logs,omitempty"`    // keyed by absolute log path
	HTTP       map[string]string    `json:"http,omitempty"`    // file name -> sha256
	Snapshots  map[string]string    `json:"snapshots,omitempty"`
}

type logState struct {
	Offset int64  `json:"offset"` // bytes already bundled
	Dev    uint64 `json:"dev,omitempty"`
	Ino    uint64 `json:"ino,omitempty"`
	HasID  bool   `json:"has_id,omitempty"`
}

func newState() *State {
	return &State{Logs: map[string]*logState{}, HTTP: map[string]string{}, Snapshots: map[string]string{}}
}

// loadState returns an empty state if the file is missing; if it is
// corrupt it also returns an error for the caller to warn about.
func loadState(path string) (*State, error) {
	s := newState()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err == nil {
		err = json.Unmarshal(data, s)
	}
	if err != nil {
		return newState(), fmt.Errorf("unreadable state %s: %v", path, err)
	}
	if s.Logs == nil {
		s.Logs = map[string]*logState{}
	}
	if s.HTTP == nil {
		s.HTTP = map[string]string{}
	}
	if s.Snapshots == nil {
		s.Snapshots = map[string]string{}
	}
	return s, nil
}

// save writes the state atomically.
func (s *State) save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// next returns a copy to record the outcome of a new bundle in.
func (s *State) next() *State {
	n := newState()
	n.LastBundle = s.LastBundle
	n.Pending = append([]string(nil), s.Pending...)
	for k, v := range s.Logs {
		c := *v
		n.Logs[k] = &c
	}
	for k, v := range s.HTTP {
		n.HTTP[k] = v
	}
	for k, v := range s.Snapshots {
		n.Snapshots[k] = v
	}
	return n
}
