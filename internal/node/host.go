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
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// memoryReserve is held back from node capacity for macOS and the control plane.
const memoryReserve = 2 << 30

// HostFacts are the static properties of the Mac reported in Node status.
type HostFacts struct {
	CPUs          int
	MemoryBytes   int64
	DiskBytes     int64
	OSImage       string
	KernelVersion string
	BootID        string
	SystemUUID    string
	RuntimeVer    string
}

func readHostFacts(stateDir, hullBin string) (HostFacts, error) {
	var f HostFacts
	f.CPUs = runtime.NumCPU()
	mem, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return f, fmt.Errorf("sysctl hw.memsize: %w", err)
	}
	f.MemoryBytes = int64(mem) - memoryReserve
	var st unix.Statfs_t
	if err := unix.Statfs(stateDir, &st); err == nil {
		f.DiskBytes = int64(st.Blocks) * int64(st.Bsize)
	}
	var u unix.Utsname
	if err := unix.Uname(&u); err == nil {
		f.KernelVersion = unix.ByteSliceToString(u.Release[:])
	}
	f.OSImage = osImage()
	f.BootID, _ = unix.Sysctl("kern.bootsessionuuid")
	f.SystemUUID = platformUUID()
	f.RuntimeVer = hullVersion(hullBin)
	return f, nil
}

func run(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func osImage() string {
	name, ver, build := run("sw_vers", "-productName"), run("sw_vers", "-productVersion"), run("sw_vers", "-buildVersion")
	if name == "" {
		return "macOS"
	}
	return fmt.Sprintf("%s %s (%s)", name, ver, build)
}

var uuidRE = regexp.MustCompile(`"IOPlatformUUID" = "([0-9A-Fa-f-]+)"`)

func platformUUID() string {
	m := uuidRE.FindStringSubmatch(run("ioreg", "-rd1", "-c", "IOPlatformExpertDevice"))
	if m == nil {
		return ""
	}
	return m[1]
}

// hullVersion reports the runtime as hull://<version>, as kubelets report
// containerd://<version>.
func hullVersion(bin string) string {
	fields := strings.Fields(run(bin, "--version"))
	if len(fields) >= 2 {
		return "hull://" + strings.TrimPrefix(fields[1], "v")
	}
	return "hull://unknown"
}
