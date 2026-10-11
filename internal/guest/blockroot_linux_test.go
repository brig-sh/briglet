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
	"strings"
	"testing"
)

// A container without a disk is left alone.
func TestUseBlockRootIgnoresShares(t *testing.T) {
	c := Container{Name: "a", Rootfs: "/run/briglet/images/x"}
	on, err := useBlockRoot(&c)
	if on || err != nil || c.UpperBase != "" || c.Rootfs != "/run/briglet/images/x" {
		t.Errorf("on=%t err=%v container=%+v", on, err, c)
	}
}

// A disk that was not mounted falls back to the share, and with no share the
// container cannot start.
func TestUseBlockRootFallsBack(t *testing.T) {
	blockRoots.Lock()
	blockRoots.lowers, blockRoots.scratch = map[string]string{}, false
	blockRoots.Unlock()

	c := Container{Name: "a", RootfsDisk: "disk2", Rootfs: "/run/briglet/images/x"}
	on, err := useBlockRoot(&c)
	if on || err != nil || c.UpperBase != "" || c.Rootfs != "/run/briglet/images/x" {
		t.Errorf("with a share: on=%t err=%v container=%+v", on, err, c)
	}
	c = Container{Name: "a", RootfsDisk: "disk2"}
	if _, err := useBlockRoot(&c); err == nil || !strings.Contains(err.Error(), "disk2") {
		t.Errorf("without a share: err = %v, want a refusal that names the disk", err)
	}
}
