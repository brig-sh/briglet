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
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/brig-sh/briglet/internal/hull"
)

// Mke2fs is where Homebrew's e2fsprogs keeps mke2fs, which hull runs to build
// the ext4 image of each container image.
const Mke2fs = "/opt/homebrew/opt/e2fsprogs/sbin/mke2fs"

// maxImageDisks is how many container images a pod can have as disks. hvi
// attaches four disks; the overlay-block root takes two. Containers of any
// further image run from a share.
const maxImageDisks = 2

// diskPlan decides which images of a pod are disks in overlay-block mode, and
// under which serial the guest finds them. The root pair is disk0 (the base
// image, read-only) and disk1 (the instance's writable disk); the images hull
// is asked for with --image-disk follow as disk2 and disk3, in the order they
// are first named.
type diskPlan struct {
	on         bool
	baseDigest string
	usesBase   bool
	digests    []string
	// refs are the --image-disk arguments, parallel to digests.
	refs []string
}

func newDiskPlan(on bool, baseDigest string) *diskPlan {
	return &diskPlan{on: on, baseDigest: baseDigest}
}

// assign returns the serial of the disk that holds the image, or "" when the
// image stays a share. The base image is already disk0.
func (d *diskPlan) assign(digest, ref string) string {
	if !d.on {
		return ""
	}
	if digest == d.baseDigest {
		d.usesBase = true
		return hull.Serial(0)
	}
	for i, have := range d.digests {
		if have == digest {
			return hull.Serial(2 + i)
		}
	}
	if len(d.digests) >= maxImageDisks {
		return ""
	}
	d.digests = append(d.digests, digest)
	d.refs = append(d.refs, ref)
	return hull.Serial(2 + len(d.digests) - 1)
}

// scratch is the serial of the writable disk the containers' upper layers go
// on, or "" when no container uses a disk. It is the root's upper, disk1.
func (d *diskPlan) scratch() string {
	if d.on && (d.usesBase || len(d.digests) > 0) {
		return hull.Serial(1)
	}
	return ""
}

// verifyDisks checks hull's record of the instance's disks against the plan:
// the guest picks disks by serial from pod.json, which was written before the
// VM existed, so a hull that numbered them differently would run a container
// from the wrong image.
func verifyDisks(h *hull.Client, instance string, spec hull.RunSpec, baseDigest string, d *diskPlan) error {
	recs, err := h.Disks(instance)
	if err != nil {
		return fmt.Errorf("hull did not record the disks of %s: %w", instance, err)
	}
	bySerial := map[string]hull.DiskRecord{}
	for _, r := range recs {
		bySerial[r.Serial] = r
	}
	if r := bySerial[hull.Serial(0)]; r.Digest != baseDigest {
		return fmt.Errorf("disk0 of %s holds %q, expected the base image %s", instance, r.Digest, baseDigest)
	}
	for i, digest := range d.digests {
		serial := spec.ImageDiskSerial(i)
		if r := bySerial[serial]; r.Digest != digest {
			return fmt.Errorf("%s of %s holds %q, expected %s", serial, instance, r.Digest, digest)
		}
	}
	return nil
}

// checkOverlayBlock fails early, with the fix, when the host cannot run pods
// in overlay-block mode.
func checkOverlayBlock(ctx context.Context, h *hull.Client) error {
	if _, err := os.Stat(Mke2fs); err != nil {
		return fmt.Errorf("BRIGLET_ROOTFS_MODE=overlay-block needs mke2fs at %s: %w (brew install e2fsprogs)", Mke2fs, err)
	}
	if !h.SupportsImageDisk(ctx) {
		return fmt.Errorf("BRIGLET_ROOTFS_MODE=overlay-block needs a hull with `run --image-disk`: %s has none (set HULL_BIN to hull's feat/overlay-block-rootfs build)", h.Bin)
	}
	if h.BootAssets == "" {
		log.Printf("overlay-block: HULL_BOOT_ASSETS is not set, so hull boots its published assets, whose initrd must carry the block-lower init")
		return nil
	}
	for _, f := range []string{"Image", "container-initrd"} {
		if _, err := os.Stat(filepath.Join(h.BootAssets, f)); err != nil {
			return fmt.Errorf("HULL_BOOT_ASSETS %s: %w", h.BootAssets, err)
		}
	}
	return nil
}
