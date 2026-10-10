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

//go:build linux

package guest

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// defaultCaps is the default capability set the kubelet gives a
// non-privileged container: enough to run as a normal root user, without
// CAP_NET_ADMIN, CAP_SYS_ADMIN or the ptrace and scheduling powers.
var defaultCaps = []uintptr{
	unix.CAP_AUDIT_WRITE, unix.CAP_CHOWN, unix.CAP_DAC_OVERRIDE, unix.CAP_FOWNER,
	unix.CAP_FSETID, unix.CAP_KILL, unix.CAP_MKNOD, unix.CAP_NET_BIND_SERVICE,
	unix.CAP_NET_RAW, unix.CAP_SETFCAP, unix.CAP_SETGID, unix.CAP_SETPCAP,
	unix.CAP_SETUID, unix.CAP_SYS_CHROOT,
}

// dropCaps limits the calling thread's capability bounding set to the
// default caps. A process exec'd from the thread, root included, cannot
// then take a capability outside the set. It runs on a thread that is
// never handed back: the forked container child, or an exec thread that
// already left the supervisor's namespaces.
func dropCaps() error {
	keep := make(map[uintptr]bool, len(defaultCaps))
	for _, c := range defaultCaps {
		keep[c] = true
	}
	for c := uintptr(0); c <= lastCap(); c++ {
		if keep[c] {
			continue
		}
		err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0)
		if err == unix.EPERM {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// lastCap reads the kernel's highest capability number.
func lastCap() uintptr {
	if b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			return uintptr(n)
		}
	}
	return 40
}
