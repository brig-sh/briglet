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

package hull

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestRunArgsSharesAndSecretEnv(t *testing.T) {
	args := RunArgs(RunSpec{
		Name: "x", Image: "busybox", Pull: "missing", MemMB: 512, CPUs: 1,
		GatewaySock: "/s", GatewayCIDR: "10.87.0.10/24",
		Env:       []string{"PLAIN=visible"},
		SecretEnv: []string{"TOKEN=hidden-value"},
		Shares: []Share{
			{Host: "/h/cm", Guest: "/etc/cm", ReadOnly: true},
			{Host: "/h/tmp", Guest: "/tmp/x"},
		},
		Argv: []string{"sh", "-c", "true"},
	})
	line := strings.Join(args, " ")
	if strings.Contains(line, "hidden-value") {
		t.Fatalf("secret value on the command line: %s", line)
	}
	for _, want := range []string{"--env PLAIN=visible", "--env TOKEN ", "--shared-dir /h/cm:/etc/cm:ro", "--shared-dir /h/tmp:/tmp/x:rw"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
	dash := slices.Index(args, "--")
	if dash < 0 || slices.Index(args, "--shared-dir") > dash || args[dash+1] != "busybox" {
		t.Errorf("flags must precede -- and the image: %s", line)
	}
}

func TestNormalizeRef(t *testing.T) {
	cases := map[string]string{
		"busybox":                        "docker.io/library/busybox:latest",
		"busybox:1.28":                   "docker.io/library/busybox:1.28",
		"user/app":                       "docker.io/user/app:latest",
		"registry.k8s.io/pause:3.10":     "registry.k8s.io/pause:3.10",
		"localhost:5000/app":             "localhost:5000/app:latest",
		"nginx@sha256:abc":               "docker.io/library/nginx@sha256:abc",
		"docker.io/library/nginx:alpine": "docker.io/library/nginx:alpine",
	}
	for in, want := range cases {
		if got := NormalizeRef(in); got != want {
			t.Errorf("NormalizeRef(%q) = %q, want %q", in, got, want)
		}
	}
	if got := MirrorRef("busybox", "mirror.gcr.io"); got != "mirror.gcr.io/library/busybox:latest" {
		t.Errorf("MirrorRef(busybox) = %q", got)
	}
	if got := MirrorRef("registry.k8s.io/pause:3.10", "mirror.gcr.io"); got != "registry.k8s.io/pause:3.10" {
		t.Errorf("MirrorRef(registry.k8s.io) = %q", got)
	}
	if got := MirrorRef("busybox", ""); got != "busybox" {
		t.Errorf("MirrorRef without a mirror = %q", got)
	}
}

// Image disks are flags before the --, and their serials follow the root's
// pair when the root is overlay-block.
func TestRunArgsImageDisks(t *testing.T) {
	spec := RunSpec{
		Name: "x", Image: "busybox", Pull: "missing", MemMB: 64, CPUs: 1,
		GatewaySock: "/s", GatewayCIDR: "10.87.0.10/24",
		OverlayBlock: true,
		ImageDisks:   []string{"nginx:alpine", "redis"},
		Argv:         []string{"sleep", "1"},
	}
	args := RunArgs(spec)
	line := strings.Join(args, " ")
	for _, want := range []string{"--image-disk nginx:alpine", "--image-disk redis"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
	dash := slices.Index(args, "--")
	if dash < 0 || slices.Index(args, "--image-disk") > dash || args[dash+1] != "busybox" {
		t.Errorf("flags must precede -- and the image: %s", line)
	}
	if spec.ImageDiskSerial(0) != "disk2" || spec.ImageDiskSerial(1) != "disk3" {
		t.Errorf("serials %s %s, want disk2 disk3", spec.ImageDiskSerial(0), spec.ImageDiskSerial(1))
	}
	spec.OverlayBlock = false
	if spec.ImageDiskSerial(0) != "disk0" {
		t.Errorf("without an overlay-block root the first image is %s, want disk0", spec.ImageDiskSerial(0))
	}
}

// The overlay-block mode and the boot assets travel in hull's environment.
func TestRunEnvCarriesRootfsModeAndBootAssets(t *testing.T) {
	dir := t.TempDir()
	out := dir + "/env"
	script := dir + "/hull"
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+out+"\necho id\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := New(script, dir)
	c.BootAssets = "/assets"
	if _, err := c.Run(context.Background(), RunSpec{Name: "x", Image: "i", OverlayBlock: true, SecretEnv: []string{"K=v"}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"HULL_ROOTFS_MODE=overlay-block", "HULL_BOOT_ASSETS=/assets", "K=v"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("hull did not see %s", want)
		}
	}
	if _, err := c.Run(context.Background(), RunSpec{Name: "y", Image: "i"}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(out)
	if strings.Contains(string(b), "HULL_ROOTFS_MODE=overlay-block") && os.Getenv("HULL_ROOTFS_MODE") == "" {
		t.Error("the default mode set HULL_ROOTFS_MODE")
	}
}
