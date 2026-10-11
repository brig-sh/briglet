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

package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brig-sh/briglet/internal/hull"
)

// The base image is disk0, further images follow the root pair as disk2 and
// disk3 in the order they are first named, a repeated image keeps its disk,
// and an image past the limit stays a share.
func TestDiskPlanAssignsSerials(t *testing.T) {
	d := newDiskPlan(true, "sha256:base")
	cases := []struct{ digest, want string }{
		{"sha256:base", "disk0"},
		{"sha256:nginx", "disk2"},
		{"sha256:base", "disk0"},
		{"sha256:redis", "disk3"},
		{"sha256:nginx", "disk2"},
		{"sha256:third", ""},
	}
	for _, c := range cases {
		if got := d.assign(c.digest, c.digest+"-ref"); got != c.want {
			t.Errorf("assign(%s) = %q, want %q", c.digest, got, c.want)
		}
	}
	if len(d.refs) != 2 || d.refs[0] != "sha256:nginx-ref" || d.refs[1] != "sha256:redis-ref" {
		t.Errorf("--image-disk refs %v", d.refs)
	}
	if d.scratch() != "disk1" {
		t.Errorf("scratch disk %q, want disk1", d.scratch())
	}
}

// Off, the plan names no disks and no scratch disk, so pods stay as before.
func TestDiskPlanOffKeepsShares(t *testing.T) {
	d := newDiskPlan(false, "sha256:base")
	if got := d.assign("sha256:base", "x"); got != "" {
		t.Errorf("assign = %q, want a share", got)
	}
	if d.scratch() != "" || len(d.refs) != 0 {
		t.Error("an off plan asked for disks")
	}
}

// A pod with no container on a disk needs no scratch disk.
func TestDiskPlanScratchOnlyWhenUsed(t *testing.T) {
	d := newDiskPlan(true, "sha256:base")
	if d.scratch() != "" {
		t.Error("scratch disk for a pod without disks")
	}
}

func writeDisksJSON(t *testing.T, store, instance string, recs []hull.DiskRecord) {
	t.Helper()
	dir := filepath.Join(store, "instances", instance)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"disks": recs})
	if err := os.WriteFile(filepath.Join(dir, "disks.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The check refuses a hull that numbered the disks differently from the plan,
// because pod.json is written before the VM exists.
func TestVerifyDisks(t *testing.T) {
	store := t.TempDir()
	h := hull.New("hull", store)
	d := newDiskPlan(true, "sha256:base")
	d.assign("sha256:nginx", "nginx")
	spec := hull.RunSpec{OverlayBlock: true, ImageDisks: d.refs}
	good := []hull.DiskRecord{
		{Serial: "disk0", Role: "rootfs-lower", Digest: "sha256:base"},
		{Serial: "disk1", Role: "rootfs-upper"},
		{Serial: "disk2", Role: "image-lower", Digest: "sha256:nginx"},
	}
	writeDisksJSON(t, store, "ok", good)
	if err := verifyDisks(h, "ok", spec, "sha256:base", d); err != nil {
		t.Errorf("matching layout refused: %v", err)
	}
	bad := append([]hull.DiskRecord{}, good...)
	bad[2].Digest = "sha256:other"
	writeDisksJSON(t, store, "bad", bad)
	if err := verifyDisks(h, "bad", spec, "sha256:base", d); err == nil || !strings.Contains(err.Error(), "disk2") {
		t.Errorf("mismatched layout: err = %v, want a refusal naming disk2", err)
	}
	if err := verifyDisks(h, "missing", spec, "sha256:base", d); err == nil {
		t.Error("no disks.json was accepted")
	}
}

func TestCheckRootfsMode(t *testing.T) {
	for _, ok := range []string{"", "share", "overlay-block"} {
		if err := CheckRootfsMode(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	if err := CheckRootfsMode("overlay"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}
