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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Proc is one supervised child process, identified by a pid file under run/.
type Proc struct {
	Name string
	Bin  string
	Args []string
	Env  []string

	pidFile string
	logFile string
	exited  chan struct{}
	waitErr error
}

// NewProc binds a component name to its pid and log files.
func NewProc(p Paths, name, bin string, args ...string) *Proc {
	return &Proc{
		Name:    name,
		Bin:     bin,
		Args:    args,
		pidFile: filepath.Join(p.Run, name+".pid"),
		logFile: filepath.Join(p.Logs, name+".log"),
	}
}

// LogFile returns the path the process writes stdout and stderr to.
func (pr *Proc) LogFile() string { return pr.logFile }

// Start launches the process in its own session so it outlives `briglet up`,
// and records its pid and executable in the pid file.
func (pr *Proc) Start() error {
	lf, err := os.OpenFile(pr.logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	fmt.Fprintf(lf, "\n==== briglet: starting %s at %s\n%s %s\n", pr.Name, time.Now().Format(time.RFC3339), pr.Bin, strings.Join(pr.Args, " "))
	cmd := exec.Command(pr.Bin, pr.Args...)
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.Env = append(os.Environ(), pr.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", pr.Name, err)
	}
	pr.exited = make(chan struct{})
	go func() {
		pr.waitErr = cmd.Wait()
		close(pr.exited)
	}()
	content := fmt.Sprintf("%d\n%s\n", cmd.Process.Pid, pr.Bin)
	if err := os.WriteFile(pr.pidFile, []byte(content), 0o600); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	return nil
}

// Exited is closed when a process started by this Proc exits. It is nil for
// processes adopted from a pid file.
func (pr *Proc) Exited() <-chan struct{} { return pr.exited }

// ExitErr describes how the process exited, once Exited is closed.
func (pr *Proc) ExitErr() error { return pr.waitErr }

// Pid returns the live pid recorded in the pid file, or 0 when the file is
// missing, stale, or names a process that is not ours.
func (pr *Proc) Pid() int {
	b, err := os.ReadFile(pr.pidFile)
	if err != nil {
		return 0
	}
	lines := strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)
	if len(lines) != 2 {
		return 0
	}
	pid, err := strconv.Atoi(lines[0])
	if err != nil || pid <= 1 {
		return 0
	}
	if !isOurs(pid, lines[1]) {
		return 0
	}
	return pid
}

// isOurs checks that pid is alive and runs the executable we recorded, so a
// recycled pid is never signaled.
func isOurs(pid int, bin string) bool {
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=", "-o", "comm=").Output()
	if err != nil {
		return false
	}
	stat, comm, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	if strings.HasPrefix(stat, "Z") {
		return false
	}
	return strings.TrimSpace(comm) == bin
}

// Stop sends SIGTERM, waits up to grace, then SIGKILL. It only signals the
// pid from our own pid file, and removes the file afterwards.
func (pr *Proc) Stop(grace time.Duration) (bool, error) {
	pid := pr.Pid()
	if pid == 0 {
		_ = os.Remove(pr.pidFile)
		return false, nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return true, fmt.Errorf("signal %s (%d): %w", pr.Name, pid, err)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if pr.Pid() == 0 {
			_ = os.Remove(pr.pidFile)
			return true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	for i := 0; i < 50 && pr.Pid() != 0; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	_ = os.Remove(pr.pidFile)
	return true, nil
}

// TailLog returns the last n lines of the process log.
func (pr *Proc) TailLog(n int) string {
	b, err := os.ReadFile(pr.logFile)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
