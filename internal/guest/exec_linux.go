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
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ExecOptions override what a command run in a container inherits from the
// container.
type ExecOptions struct {
	Container string
	// User is uid[:gid]; empty keeps the container's.
	User string
	Cwd  string
	Env  []string
}

// Exec runs argv inside a running container: in its pid and mount
// namespaces and its root, with its user, environment and working
// directory unless opts override them. It returns the command's exit code.
// It works from the guest agent, which runs as root in the VM's own
// namespaces.
func Exec(opts ExecOptions, argv []string) (int, error) {
	var pod Pod
	if err := readJSON(PodFile, &pod); err != nil {
		return 0, err
	}
	var st Status
	if err := readJSON(StatusFile, &st); err != nil {
		return 0, err
	}
	var spec *Container
	for i := range pod.Containers {
		if pod.Containers[i].Name == opts.Container {
			spec = &pod.Containers[i]
		}
	}
	cs := st.Find(opts.Container)
	if spec == nil || cs == nil {
		return 0, fmt.Errorf("no container %q in pod %s", opts.Container, pod.Name)
	}
	if cs.State != StateRunning || cs.PID == 0 {
		return 0, fmt.Errorf("container %s is not running", opts.Container)
	}
	uid, gid, groups := spec.UID, spec.GID, spec.Groups
	if opts.User != "" {
		u, g, hasGroup := strings.Cut(opts.User, ":")
		n, err := strconv.ParseUint(u, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("user %q: only numeric ids are supported", opts.User)
		}
		uid, groups = uint32(n), nil
		if hasGroup {
			m, err := strconv.ParseUint(g, 10, 32)
			if err != nil {
				return 0, fmt.Errorf("user %q: only numeric ids are supported", opts.User)
			}
			gid = uint32(m)
		}
	}
	env := mergeEnv(spec.Env, opts.Env)
	tty := isTerminal(0)
	if tty && !hasEnv(env, "TERM") {
		env = append(env, "TERM=xterm")
	}
	env = withPath(env)
	cwd := opts.Cwd
	if cwd == "" {
		cwd = spec.Cwd
	}
	if cwd == "" {
		cwd = "/"
	}

	proc := "/proc/" + strconv.Itoa(cs.PID)
	rootFD, err := unix.Open(proc+"/root", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("container %s: %w", opts.Container, err)
	}
	mntFD, err := unix.Open(proc+"/ns/mnt", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	pidFD, err := unix.Open(proc+"/ns/pid", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	// Mount namespaces belong to a thread's fs_struct, which Go threads
	// share. This thread takes a private copy, joins the container's
	// namespaces and its root, forks the command and is never handed back.
	runtime.LockOSThread()
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return 0, fmt.Errorf("unshare fs: %w", err)
	}
	if err := unix.Setns(mntFD, unix.CLONE_NEWNS); err != nil {
		return 0, fmt.Errorf("join the mount namespace: %w", err)
	}
	if !pod.ShareProcessNamespace {
		if err := unix.Setns(pidFD, unix.CLONE_NEWPID); err != nil {
			return 0, fmt.Errorf("join the pid namespace: %w", err)
		}
	}
	// The thread never returns to the supervisor, so its bounding set
	// can drop to what the container itself would have.
	if err := dropCaps(); err != nil {
		return 0, fmt.Errorf("drop capabilities: %w", err)
	}
	if err := unix.Fchdir(rootFD); err != nil {
		return 0, err
	}
	if err := unix.Chroot("."); err != nil {
		return 0, err
	}
	if err := unix.Chdir(cwd); err != nil {
		return 0, fmt.Errorf("chdir to %q: %w", cwd, err)
	}
	path, err := lookPath(argv, env)
	if err != nil {
		return 127, err
	}
	gs := make([]uint32, len(groups))
	copy(gs, groups)
	attr := &syscall.ProcAttr{
		Env:   env,
		Files: []uintptr{0, 1, 2},
		Sys: &syscall.SysProcAttr{
			Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: gs},
			Setsid:     tty,
			Setctty:    tty,
		},
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2)
	if tty {
		// The command takes the terminal as its controlling tty after it
		// dropped to the container's user, which may not steal it from
		// this session. Give it up here; the SIGHUP that follows is ours.
		if err := unix.IoctlSetInt(0, unix.TIOCNOTTY, 0); err == nil {
			drainHangup(sigs)
		}
	}
	pid, err := syscall.ForkExec(path, argv, attr)
	if err != nil {
		code := 126
		if err == syscall.ENOENT {
			code = 127
		}
		return code, fmt.Errorf("exec %q: %w", argv[0], err)
	}
	go func() {
		for sig := range sigs {
			_ = syscall.Kill(pid, sig.(syscall.Signal))
		}
	}()
	for {
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(pid, &ws, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return ws.ExitStatus(), nil
	}
}

// drainHangup swallows the SIGHUP that giving up the controlling tty
// sends, waiting briefly for it.
func drainHangup(sigs chan os.Signal) {
	t := time.NewTimer(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				return
			}
		case <-t.C:
			return
		}
	}
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func isTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}
