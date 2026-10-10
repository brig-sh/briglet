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

package cp

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GuestBinaryName is the supervisor every pod VM runs.
const GuestBinaryName = "briglet-guest"

// prepareGuestBinary makes sure bin/briglet-guest next to the briglet
// executable exists and is not older than its sources, building it when it
// is stale and go is on PATH, and copies it into the state dir, which
// every pod VM shares. Pods that run keep the copy they booted with.
func (c *Cluster) prepareGuestBinary(int) error {
	src := filepath.Join(filepath.Dir(c.Self), GuestBinaryName)
	repo := filepath.Dir(filepath.Dir(c.Self))
	newest := newestSource(repo)
	info, err := os.Stat(src)
	missing := err != nil
	stale := missing || (!newest.IsZero() && info.ModTime().Before(newest))
	if stale {
		goBin, lookErr := exec.LookPath("go")
		switch {
		case !newest.IsZero() && lookErr == nil:
			if err := buildGuest(goBin, repo, src); err != nil {
				return err
			}
		case missing:
			return fmt.Errorf("%s is missing; build it with `make guest`", src)
		default:
			return fmt.Errorf("%s is older than its sources in %s; rebuild it with `make guest`", src, repo)
		}
	}
	return copyIfChanged(src, filepath.Join(c.Paths.GuestBin, GuestBinaryName))
}

// newestSource is the latest modification time of the guest sources in a
// briglet checkout, zero when repo is not one.
func newestSource(repo string) time.Time {
	var newest time.Time
	for _, dir := range []string{"cmd/briglet-guest", "internal/guest"} {
		_ = filepath.WalkDir(filepath.Join(repo, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			if info, err := d.Info(); err == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
			return nil
		})
	}
	if newest.IsZero() {
		return newest
	}
	for _, f := range []string{"go.mod", "go.sum"} {
		if info, err := os.Stat(filepath.Join(repo, f)); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

func buildGuest(goBin, repo, out string) error {
	cmd := exec.Command(goBin, "build", "-trimpath", "-o", out, "./cmd/briglet-guest")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &errb, &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build %s: %w: %s", out, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// copyIfChanged copies src to dst through a rename unless dst has the
// same content.
func copyIfChanged(src, dst string) error {
	want, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if have, err := os.ReadFile(dst); err == nil && bytes.Equal(have, want) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, bytes.NewReader(want)); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
