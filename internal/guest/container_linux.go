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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ContainerCommand is the hidden subcommand the supervisor re-executes
// itself with to become a container's first process.
const ContainerCommand = "container-init"

// The kubelet's default masked and read-only paths (runc defaults).
var (
	maskedPaths = []string{
		"/proc/acpi", "/proc/asound", "/proc/kcore", "/proc/keys",
		"/proc/latency_stats", "/proc/timer_list", "/proc/timer_stats", "/proc/sched_debug",
		"/proc/scsi", "/sys/firmware", "/sys/devices/virtual/powercap",
	}
	readOnlyPaths = []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"}
)

// startContainer starts the container's first process in new pid and mount
// namespaces, with its output going to the container's log. It returns once
// the process has exec'd the container's command, or with the error that
// stopped it; a non-zero pid with an error is a child that will exit.
func startContainer(pod *Pod, c *Container) (int, *logSink, error) {
	logs, err := newLogSink(logPath(c.Name))
	if err != nil {
		return 0, nil, err
	}
	// The container's own copy: an image disk and its upper directory are
	// per run, and a restart starts again from the spec.
	cc := *c
	if _, err := useBlockRoot(&cc); err != nil {
		return 0, logs, err
	}
	if c.TerminationLog != "" {
		// Chmod after the create, so the umask cannot strip the write bit
		// a non-root container needs to leave a termination message.
		_ = os.WriteFile(c.TerminationLog, nil, 0o666)
		_ = os.Chmod(c.TerminationLog, 0o666)
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return 0, logs, err
	}
	defer devnull.Close()
	outR, outW, err := os.Pipe()
	if err != nil {
		return 0, logs, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		return 0, logs, err
	}
	cfgR, cfgW, err := os.Pipe()
	if err != nil {
		return 0, logs, err
	}
	failR, failW, err := os.Pipe()
	if err != nil {
		return 0, logs, err
	}
	// The tmpfs must exist before the clone: the child copies the mount
	// namespace as it is then.
	cc.Mounts = podTmpfsMounts(cc.Mounts)
	flags := uintptr(syscall.CLONE_NEWNS)
	if !pod.ShareProcessNamespace {
		flags |= syscall.CLONE_NEWPID
	}
	proc, err := os.StartProcess("/proc/self/exe", []string{"briglet-guest", ContainerCommand}, &os.ProcAttr{
		Files: []*os.File{devnull, outW, errW, cfgR, failW},
		Sys: &syscall.SysProcAttr{
			Cloneflags: flags,
			Setsid:     true,
			Pdeathsig:  syscall.SIGKILL,
		},
	})
	for _, f := range []*os.File{outW, errW, cfgR, failW} {
		f.Close()
	}
	if err != nil {
		outR.Close()
		errR.Close()
		cfgW.Close()
		failR.Close()
		return 0, logs, err
	}
	pid := proc.Pid
	_ = proc.Release()
	logs.Pump(outR, "stdout")
	logs.Pump(errR, "stderr")
	_ = json.NewEncoder(cfgW).Encode(&cc)
	cfgW.Close()
	// The child closes its end on exec; anything before EOF is why it
	// could not get there.
	_ = failR.SetReadDeadline(time.Now().Add(30 * time.Second))
	msg, _ := io.ReadAll(failR)
	failR.Close()
	if len(msg) > 0 {
		return pid, logs, errors.New(strings.TrimSpace(string(msg)))
	}
	return pid, logs, nil
}

// volTmpfsDir holds one tmpfs per emptyDir of Memory medium.
const volTmpfsDir = "/run/briglet-vol"

var (
	volTmpfsMu sync.Mutex
	volTmpfs   = map[string]string{}
)

// podTmpfsMounts replaces the Memory-medium emptyDir mounts that name a
// volume with binds of one tmpfs per volume, mounted on first use in the
// supervisor's namespace. A tmpfs mounted inside each container would be
// empty again after every restart and private to its container.
func podTmpfsMounts(in []Mount) []Mount {
	out := make([]Mount, len(in))
	copy(out, in)
	for i, m := range out {
		if !m.Tmpfs || m.Volume == "" {
			continue
		}
		volTmpfsMu.Lock()
		dir, ok := volTmpfs[m.Volume]
		if !ok {
			dir = filepath.Join(volTmpfsDir, fmt.Sprintf("v%d", len(volTmpfs)))
			if err := os.MkdirAll(dir, 0o777); err == nil {
				if err = unix.Mount("tmpfs", dir, "tmpfs", 0, "mode=0777"); err == nil {
					_ = os.Chmod(dir, 0o777)
					volTmpfs[m.Volume] = dir
					ok = true
				}
			}
		}
		volTmpfsMu.Unlock()
		if ok {
			out[i].Source, out[i].Tmpfs = dir, false
		}
	}
	return out
}

// ContainerInit runs in the child: it builds the container's mounts, pivots
// into its root, drops to its user and executes its command. Errors go to
// fd 4 and end the process with 128.
func ContainerInit() {
	cfgF, failF := os.NewFile(3, "config"), os.NewFile(4, "fail")
	var c Container
	err := json.NewDecoder(cfgF).Decode(&c)
	cfgF.Close()
	if err == nil {
		err = setupAndExec(&c)
	}
	fmt.Fprintf(failF, "%v", err)
	os.Exit(128)
}

func setupAndExec(c *Container) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}
	base := filepath.Join(scratchDir, c.Name)
	if c.UpperBase != "" {
		// The writable layer is a directory of the scratch disk, prepared
		// by the supervisor.
		base = c.UpperBase
	} else {
		if err := os.MkdirAll(base, 0o700); err != nil {
			return err
		}
		mem := c.MemoryMB
		if mem <= 0 {
			mem = 512
		}
		if err := unix.Mount("tmpfs", base, "tmpfs", 0, fmt.Sprintf("size=%dm,mode=0700", mem)); err != nil {
			return fmt.Errorf("tmpfs for the writable layer: %w", err)
		}
	}
	root := filepath.Join(base, "root")
	for _, d := range []string{"upper", "work", "root"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			return err
		}
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s/upper,workdir=%s/work", c.Rootfs, base, base)
	if err := unix.Mount("overlay", root, "overlay", 0, opts); err != nil {
		return fmt.Errorf("overlay on %s: %w", c.Rootfs, err)
	}
	if err := systemMounts(root); err != nil {
		return err
	}
	for _, m := range c.Mounts {
		if m.Tmpfs {
			if err := tmpfsMount(root, m); err != nil {
				return fmt.Errorf("tmpfs at %s: %w", m.Destination, err)
			}
			continue
		}
		if err := bindMount(root, m); err != nil {
			return fmt.Errorf("mount %s at %s: %w", m.Source, m.Destination, err)
		}
	}
	if !c.Privileged {
		maskPaths(root)
	}
	cwd := c.Cwd
	if cwd == "" {
		cwd = "/"
	}
	if p, err := resolveIn(root, cwd); err == nil {
		_ = os.MkdirAll(p, 0o755)
	}
	if c.ReadOnlyRootfs {
		if err := unix.Mount("", root, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("read-only root: %w", err)
		}
	}
	if err := enterRoot(root); err != nil {
		return err
	}
	if err := unix.Chdir(cwd); err != nil {
		return fmt.Errorf("chdir to cwd (%q): %w", cwd, err)
	}
	if !c.Privileged {
		// The bounding set survives exec, so this is the last point the
		// container's capabilities can be limited.
		if err := dropCaps(); err != nil {
			return fmt.Errorf("drop capabilities: %w", err)
		}
	}
	if c.NoNewPrivs {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("no new privs: %w", err)
		}
	}
	if err := setUser(c.UID, c.GID, c.Groups); err != nil {
		return err
	}
	env := withPath(c.Env)
	argv0, err := lookPath(c.Argv, env)
	if err != nil {
		return err
	}
	unix.CloseOnExec(4)
	return fmt.Errorf("exec %q: %w", c.Argv[0], syscall.Exec(argv0, c.Argv, env))
}

// systemMounts gives the root a /proc of its own pid namespace, the VM's
// /sys read-only and the VM's /dev, which carries /dev/pts and the pod's
// /dev/shm.
func systemMounts(root string) error {
	dirs := map[string]string{}
	for _, d := range []string{"proc", "sys", "dev"} {
		p, err := resolveIn(root, "/"+d)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			return err
		}
		dirs[d] = p
	}
	if err := unix.Mount("proc", dirs["proc"], "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}
	sys := dirs["sys"]
	if err := unix.Mount("/sys", sys, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("mount /sys: %w", err)
	}
	if err := unix.Mount("", sys, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("remount /sys read-only: %w", err)
	}
	if err := unix.Mount("/dev", dirs["dev"], "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("mount /dev: %w", err)
	}
	return nil
}

// tmpfsMount mounts a fresh tmpfs at m.Destination inside root, the
// kubelet's emptyDir of Memory medium: a tmpfs with mode 0777.
func tmpfsMount(root string, m Mount) error {
	dst, err := resolveIn(root, m.Destination)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o777); err != nil {
		return err
	}
	flags := uintptr(0)
	if m.ReadOnly {
		flags |= unix.MS_RDONLY
	}
	if err := unix.Mount("tmpfs", dst, "tmpfs", flags, "mode=0777"); err != nil {
		return err
	}
	return os.Chmod(dst, 0o777)
}

// bindMount binds m.Source at m.Destination inside root, creating the
// mount point to match the source: a directory or an empty file.
func bindMount(root string, m Mount) error {
	info, err := os.Stat(m.Source)
	if err != nil {
		return err
	}
	dst, err := resolveIn(root, m.Destination)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if st, err := os.Lstat(dst); err == nil && st.IsDir() {
			return fmt.Errorf("%s is a directory and the source is a file", m.Destination)
		} else if errors.Is(err, fs.ErrNotExist) {
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			f.Close()
		}
	}
	if err := unix.Mount(m.Source, dst, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return err
	}
	if m.ReadOnly {
		return unix.Mount("", dst, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "")
	}
	return nil
}

// maskPaths hides and write-protects the kernel interfaces the kubelet's
// default profile does. A path the kernel does not have is skipped.
func maskPaths(root string) {
	for _, p := range maskedPaths {
		t := filepath.Join(root, p)
		st, err := os.Stat(t)
		switch {
		case err != nil:
		case st.IsDir():
			_ = unix.Mount("tmpfs", t, "tmpfs", unix.MS_RDONLY, "size=0")
		default:
			_ = unix.Mount("/dev/null", t, "", unix.MS_BIND, "")
		}
	}
	for _, p := range readOnlyPaths {
		t := filepath.Join(root, p)
		if _, err := os.Stat(t); err != nil {
			continue
		}
		if unix.Mount(t, t, "", unix.MS_BIND|unix.MS_REC, "") == nil {
			_ = unix.Mount("", t, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "")
		}
	}
}

// enterRoot makes root the process's root with pivot_root, and falls back
// to moving it over / and chroot when pivot_root refuses.
func enterRoot(root string) error {
	if err := unix.Chdir(root); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", "."); err == nil {
		if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
			return fmt.Errorf("detach the old root: %w", err)
		}
		return unix.Chdir("/")
	}
	if err := unix.Mount(root, "/", "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move the root: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return err
	}
	return unix.Chdir("/")
}

// setUser drops to the container's user. The syscall package applies the
// ids to every thread of the process.
func setUser(uid, gid uint32, groups []uint32) error {
	gs := make([]int, 0, len(groups))
	for _, g := range groups {
		gs = append(gs, int(g))
	}
	if err := syscall.Setgroups(gs); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setresgid(int(gid), int(gid), int(gid)); err != nil {
		return fmt.Errorf("setgid %d: %w", gid, err)
	}
	if err := syscall.Setresuid(int(uid), int(uid), int(uid)); err != nil {
		return fmt.Errorf("setuid %d: %w", uid, err)
	}
	return nil
}
