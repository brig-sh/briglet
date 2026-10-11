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

package guest

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// lowerMounts holds one mount point per image disk, named by serial.
	lowerMounts = "/run/briglet-lower"
	// scratchMount is where the scratch disk is mounted. A container's
	// upper layer is scratchMount/<name>.
	scratchMount = "/run/briglet-scratch"
	// diskWait is how long a disk may take to show up in /sys/block.
	diskWait = 5 * time.Second
)

// blockRoots is what the supervisor mounted for containers whose image is a
// disk: the read-only image disks by serial, and the scratch disk.
var blockRoots struct {
	sync.Mutex
	lowers  map[string]string // serial -> mount point
	scratch bool
}

// DiskBySerial returns the /dev path of the virtio-blk disk with the given
// serial, waiting up to wait for it. hull names every disk it attaches disk<N>,
// N the position on the VMM's command line, so the serial identifies a disk
// regardless of the order the kernel probed them in.
func DiskBySerial(serial string, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		files, _ := filepath.Glob("/sys/block/*/serial")
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			// The kernel NUL-terminates the serial; trim padding and the
			// newline sysfs adds.
			if strings.Trim(string(b), "\x00 \n") == serial {
				return "/dev/" + filepath.Base(filepath.Dir(f)), nil
			}
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Errorf("no virtio-blk disk with serial %s", serial)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// mountBlockRoots mounts the image disks and the scratch disk the pod's
// containers name. A disk that cannot be mounted is logged and left out; the
// containers that need it fall back to their share, or fail to start.
func mountBlockRoots(pod *Pod) {
	blockRoots.Lock()
	defer blockRoots.Unlock()
	blockRoots.lowers = map[string]string{}
	var serials []string
	seen := map[string]bool{}
	for i := range pod.Containers {
		if s := pod.Containers[i].RootfsDisk; s != "" && !seen[s] {
			seen[s] = true
			serials = append(serials, s)
		}
	}
	if len(serials) == 0 {
		return
	}
	for _, serial := range serials {
		dev, err := DiskBySerial(serial, diskWait)
		if err != nil {
			log.Printf("image disk %s: %v", serial, err)
			continue
		}
		dir := filepath.Join(lowerMounts, serial)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("image disk %s: %v", serial, err)
			continue
		}
		if err := unix.Mount(dev, dir, "ext4", unix.MS_RDONLY|unix.MS_NOATIME, ""); err != nil {
			log.Printf("image disk %s: mount %s: %v", serial, dev, err)
			continue
		}
		blockRoots.lowers[serial] = dir
	}
	if pod.ScratchDisk == "" {
		return
	}
	dev, err := DiskBySerial(pod.ScratchDisk, diskWait)
	if err != nil {
		log.Printf("scratch disk: %v", err)
		return
	}
	if err := os.MkdirAll(scratchMount, 0o700); err != nil {
		log.Printf("scratch disk: %v", err)
		return
	}
	if err := unix.Mount(dev, scratchMount, "ext4", unix.MS_NOATIME, ""); err != nil {
		log.Printf("scratch disk: mount %s: %v", dev, err)
		return
	}
	blockRoots.scratch = true
}

// useBlockRoot points a copy of c at its image disk and a fresh upper
// directory on the scratch disk. It changes nothing, and reports false, when c
// has no disk or the disk is not mounted; startContainer then uses the share.
// The upper directory is emptied first, so a restarted container starts from
// its image as it does with a tmpfs.
func useBlockRoot(c *Container) (bool, error) {
	if c.RootfsDisk == "" {
		return false, nil
	}
	blockRoots.Lock()
	lower, scratch := blockRoots.lowers[c.RootfsDisk], blockRoots.scratch
	blockRoots.Unlock()
	if lower == "" || !scratch {
		if c.Rootfs == "" {
			return false, fmt.Errorf("image disk %s is not available and the container has no share to fall back to", c.RootfsDisk)
		}
		log.Printf("container %s: disk %s is not available, using the share %s", c.Name, c.RootfsDisk, c.Rootfs)
		return false, nil
	}
	base := filepath.Join(scratchMount, c.Name)
	if err := os.RemoveAll(base); err != nil {
		return false, fmt.Errorf("clear the writable layer: %w", err)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return false, fmt.Errorf("create the writable layer: %w", err)
	}
	c.Rootfs, c.UpperBase = lower, base
	return true, nil
}
