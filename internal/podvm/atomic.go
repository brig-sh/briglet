// Copyright (c) 2026, NOFire AI
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package podvm

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FileProjection is one file of a projected volume.
type FileProjection struct {
	Data []byte
	Mode int32
}

const (
	dataLink    = "..data"
	dataLinkTmp = "..data_tmp"
	// RetainRetired is how long a replaced timestamp directory is kept. A
	// read-only hvi share lets the guest cache directory entries for 60 s,
	// so a guest can still follow the old ..data target that long after a
	// swap. Removing it at once, as the kubelet does, gives the guest ENOENT
	// until its cache expires.
	RetainRetired = 2 * time.Minute
)

// WriteAtomic writes payload into dir with the kubelet's atomic writer
// layout: the files go into a new ..<timestamp> directory, the ..data
// symlink is swapped to it with a rename, and every top-level path is a
// symlink through ..data. It reports whether anything changed; an unchanged
// payload is not rewritten.
func WriteAtomic(dir string, payload map[string]FileProjection, now time.Time) (bool, error) {
	for p := range payload {
		if err := validPayloadPath(p); err != nil {
			return false, err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	old, _ := os.Readlink(filepath.Join(dir, dataLink))
	if old != "" && samePayload(filepath.Join(dir, old), payload) {
		return false, GCAtomic(dir, now)
	}
	ts, err := os.MkdirTemp(dir, now.UTC().Format("..2006_01_02_15_04_05."))
	if err != nil {
		return false, err
	}
	if err := os.Chmod(ts, 0o755); err != nil {
		return false, err
	}
	for p, f := range payload {
		full := filepath.Join(ts, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return false, err
		}
		if err := os.WriteFile(full, f.Data, 0o600); err != nil {
			return false, err
		}
		if err := os.Chmod(full, fs.FileMode(f.Mode)&fs.ModePerm); err != nil {
			return false, err
		}
	}
	tmp := filepath.Join(dir, dataLinkTmp)
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Base(ts), tmp); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, dataLink)); err != nil {
		return false, err
	}
	if err := linkTopLevel(dir, payload); err != nil {
		return false, err
	}
	if old != "" {
		// Mark when the old directory was retired; GCAtomic counts from it.
		_ = os.Chtimes(filepath.Join(dir, old), now, now)
	}
	return true, GCAtomic(dir, now)
}

// GCAtomic removes timestamp directories that ..data stopped naming more
// than RetainRetired ago.
func GCAtomic(dir string, now time.Time) error {
	cur, _ := os.Readlink(filepath.Join(dir, dataLink))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() || !strings.HasPrefix(n, "..") || n == cur {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < RetainRetired {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, n)); err != nil {
			return err
		}
	}
	return nil
}

func validPayloadPath(p string) error {
	switch {
	case p == "" || filepath.IsAbs(p):
		return fmt.Errorf("invalid volume path %q", p)
	case strings.HasPrefix(p, ".."):
		return fmt.Errorf("volume path %q must not start with '..'", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "" {
			return fmt.Errorf("invalid volume path %q", p)
		}
	}
	return nil
}

// samePayload reports whether dir holds exactly payload, contents and
// modes.
func samePayload(dir string, payload map[string]FileProjection) bool {
	seen := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		want, ok := payload[rel]
		if !ok {
			return errors.New("extra file")
		}
		info, err := d.Info()
		if err != nil || info.Mode().Perm() != fs.FileMode(want.Mode)&fs.ModePerm {
			return errors.New("mode differs")
		}
		b, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(b, want.Data) {
			return errors.New("content differs")
		}
		seen++
		return nil
	})
	return err == nil && seen == len(payload)
}

// linkTopLevel points a symlink at ..data/<name> for every top-level name of
// the payload and removes such symlinks for names no longer there.
func linkTopLevel(dir string, payload map[string]FileProjection) error {
	want := map[string]bool{}
	for p := range payload {
		top, _, _ := strings.Cut(p, "/")
		want[top] = true
	}
	names := make([]string, 0, len(want))
	for n := range want {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		link := filepath.Join(dir, n)
		target := filepath.Join(dataLink, n)
		if cur, err := os.Readlink(link); err == nil && cur == target {
			continue
		}
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, "..") || want[n] {
			continue
		}
		if cur, err := os.Readlink(filepath.Join(dir, n)); err == nil && strings.HasPrefix(cur, dataLink+"/") {
			_ = os.Remove(filepath.Join(dir, n))
		}
	}
	return nil
}
