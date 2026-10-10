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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	backoffFirst = 10 * time.Second
	backoffMax   = 5 * time.Minute
	// backoffReset: a container that ran this long restarts without delay
	// next time, as on the kubelet.
	backoffReset = 10 * time.Minute
	// sidecarGrace is the least time a sidecar gets to stop once the
	// regular containers are gone.
	sidecarGrace = 2 * time.Second
	// scratchDir holds one mount point per container for its writable
	// layer. The mounts live in the container's own mount namespace.
	scratchDir   = "/run/briglet-ctr"
	defaultGrace = 30
)

type exitEvent struct {
	pid int
	ws  unix.WaitStatus
	at  time.Time
}

type ctlCall struct {
	req   CtlRequest
	reply chan CtlResponse
}

// ctr is the supervisor's view of one container.
type ctr struct {
	spec *Container
	// runs counts the starts in this VM.
	runs      int
	pid       int
	startedAt time.Time
	restarts  int32
	term      *Termination // the run that ended, while waiting or done
	last      *Termination // the run before the current one
	startErr  string
	logs      *logSink
	// backoff is the delay before the next restart.
	backoff   time.Duration
	nextStart time.Time
	waitMsg   string
	// done: an init container that completed, or a container that will
	// not run again.
	done bool
	// killAt is when a container asked to stop gets SIGKILL.
	killAt   time.Time
	termSent bool
}

func (c *ctr) running() bool { return c.pid != 0 }

type supervisor struct {
	pod      *Pod
	ctrs     []*ctr
	boot     string
	started  time.Time
	exits    chan exitEvent
	ctl      chan ctlCall
	stopping bool
	stopAt   time.Time
	// result is the phase the pod ends with once everything stopped; ""
	// when the agent asked for the stop.
	result      string
	message     string
	initialized bool
	final       bool
	lastStatus  []byte
}

// Run supervises the pod described by podFile until it ends or is stopped.
// It runs as the VM's main process: when it returns, the VM powers off.
func Run(podFile string) error {
	log.SetPrefix("briglet-guest: ")
	log.SetFlags(log.Lmicroseconds)
	b, err := os.ReadFile(podFile)
	if err != nil {
		return err
	}
	var pod Pod
	if err := json.Unmarshal(b, &pod); err != nil {
		return fmt.Errorf("%s: %w", podFile, err)
	}
	s := &supervisor{
		pod: &pod, started: time.Now(), boot: strconv.FormatInt(time.Now().UnixNano(), 36),
		exits: make(chan exitEvent, 64), ctl: make(chan ctlCall),
	}
	for i := range pod.Containers {
		c := &pod.Containers[i]
		s.ctrs = append(s.ctrs, &ctr{spec: c, restarts: c.RestartCount})
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		log.Printf("subreaper: %v", err)
	}
	sigchld := make(chan os.Signal, 8)
	signal.Notify(sigchld, syscall.SIGCHLD)
	term := make(chan os.Signal, 2)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)
	go s.reap(sigchld)
	s.prepareVM()
	if err := s.serveCtl(); err != nil {
		log.Printf("ctl: %v", err)
	}
	log.Printf("pod %s/%s: %d containers", pod.Namespace, pod.Name, len(pod.Containers))

	for {
		next := s.step(time.Now())
		s.writeStatus()
		if s.final {
			s.drainLogs(2 * time.Second)
			log.Printf("pod ended: %s %s", s.phase(), s.message)
			return nil
		}
		wait := time.Hour
		if !next.IsZero() {
			wait = max(time.Until(next), 10*time.Millisecond)
		}
		t := time.NewTimer(wait)
		select {
		case ev := <-s.exits:
			s.onExit(ev)
		case call := <-s.ctl:
			call.reply <- s.onCtl(call.req)
		case sig := <-term:
			log.Printf("%v: stopping the pod", sig)
			s.beginStop(time.Duration(s.grace())*time.Second, "")
		case <-t.C:
		}
		t.Stop()
	}
}

func (s *supervisor) grace() int {
	if s.pod.GraceSeconds > 0 {
		return s.pod.GraceSeconds
	}
	return defaultGrace
}

// prepareVM sets what every container shares: hostname, /dev links and
// /dev/shm, sysctls, and the log directory.
func (s *supervisor) prepareVM() {
	if s.pod.Hostname != "" {
		if err := unix.Sethostname([]byte(s.pod.Hostname)); err != nil {
			log.Printf("hostname: %v", err)
		}
	}
	// hull's /dev has no stdio links. Containers bind the VM's /dev, so
	// the links made here are theirs too.
	for link, target := range map[string]string{
		"/dev/stdin": "/proc/self/fd/0", "/dev/stdout": "/proc/self/fd/1",
		"/dev/stderr": "/proc/self/fd/2", "/dev/fd": "/proc/self/fd",
	} {
		if _, err := os.Lstat(link); err != nil {
			_ = os.Symlink(target, link)
		}
	}
	shm := s.pod.ShmMB
	if shm <= 0 {
		shm = 64
	}
	_ = os.MkdirAll("/dev/shm", 0o1777)
	if !isMountPoint("/dev/shm") {
		if err := unix.Mount("shm", "/dev/shm", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC,
			fmt.Sprintf("mode=1777,size=%dm", shm)); err != nil {
			log.Printf("/dev/shm: %v", err)
		}
	}
	// Any user may bind ports below 1024, as Docker sets it: images that
	// run as non-root and rely on file capabilities lose them in the
	// store, which does not keep security.capability.
	sysctls := map[string]string{"net.ipv4.ip_unprivileged_port_start": "0"}
	for k, v := range s.pod.Sysctls {
		sysctls[k] = v
	}
	for k, v := range sysctls {
		p := "/proc/sys/" + strings.ReplaceAll(k, ".", "/")
		if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
			log.Printf("sysctl %s: %v", k, err)
		}
	}
	if err := os.MkdirAll(LogDir, 0o755); err != nil {
		log.Printf("logs: %v", err)
	}
	_ = os.MkdirAll(scratchDir, 0o700)
}

func isMountPoint(p string) bool {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[1] == p {
			return true
		}
	}
	return false
}

func (s *supervisor) reap(sigchld chan os.Signal) {
	for range sigchld {
		for {
			var ws unix.WaitStatus
			pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			s.exits <- exitEvent{pid: pid, ws: ws, at: time.Now()}
		}
	}
}

func (s *supervisor) find(name string) *ctr {
	for _, c := range s.ctrs {
		if c.spec.Name == name {
			return c
		}
	}
	return nil
}

// step starts, restarts and stops containers as the pod's state asks and
// returns when it next needs to run.
func (s *supervisor) step(now time.Time) time.Time {
	var next time.Time
	due := func(t time.Time) {
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	for _, c := range s.ctrs {
		if c.running() && !c.killAt.IsZero() {
			if !now.Before(c.killAt) {
				_ = syscall.Kill(c.pid, syscall.SIGKILL)
				c.killAt = time.Time{}
			} else {
				due(c.killAt)
			}
		}
	}
	if s.stopping {
		return s.stepStop(now, due, next)
	}
	if !s.initialized {
		for _, c := range s.ctrs {
			if !c.spec.Init {
				continue
			}
			if c.spec.RestartAlways {
				if !c.running() {
					if !s.tryStart(c, now, due) || !c.running() {
						return next
					}
				}
				continue
			}
			if c.done {
				continue
			}
			if !c.running() {
				s.tryStart(c, now, due)
			}
			return next
		}
		s.initialized = true
	}
	allDone, failed := true, false
	for _, c := range s.ctrs {
		if c.spec.Init && !c.spec.RestartAlways {
			continue
		}
		if !c.running() && !c.done {
			s.tryStart(c, now, due)
		}
		if !c.spec.Init {
			allDone = allDone && c.done
			failed = failed || (c.done && c.term != nil && c.term.ExitCode != 0)
		}
	}
	if allDone {
		result := PhaseSucceeded
		if failed {
			result = PhaseFailed
		}
		s.beginStop(time.Duration(s.grace())*time.Second, result)
		return s.stepStop(now, due, next)
	}
	return next
}

// tryStart starts c when its back-off has passed. It reports whether it
// started it.
func (s *supervisor) tryStart(c *ctr, now time.Time, due func(time.Time)) bool {
	if now.Before(c.nextStart) {
		due(c.nextStart)
		return false
	}
	s.start(c)
	return true
}

// stepStop stops the regular containers first, then the sidecars in
// reverse order, and ends the pod when nothing runs.
func (s *supervisor) stepStop(now time.Time, due func(time.Time), next time.Time) time.Time {
	regular := false
	for _, c := range s.ctrs {
		if c.running() && (!c.spec.Init || !c.spec.RestartAlways) {
			regular = true
			s.signalStop(c, s.stopAt)
		}
	}
	if !regular {
		for i := len(s.ctrs) - 1; i >= 0; i-- {
			if c := s.ctrs[i]; c.running() {
				s.signalStop(c, maxTime(s.stopAt, now.Add(sidecarGrace)))
				break
			}
		}
	}
	for _, c := range s.ctrs {
		if c.running() {
			due(c.killAt)
			return next
		}
	}
	s.final = true
	return next
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// signalStop sends SIGTERM once and arms SIGKILL at killAt. The preStop
// handler runs first, inside the grace period the deadline sets.
func (s *supervisor) signalStop(c *ctr, killAt time.Time) {
	if !c.termSent {
		s.runPreStop(c, killAt)
		_ = syscall.Kill(c.pid, syscall.SIGTERM)
		c.termSent = true
	}
	if c.killAt.IsZero() || killAt.Before(c.killAt) {
		c.killAt = killAt
	}
}

// runPostStart runs the container's postStart handler right after the
// start, as the kubelet does, without ordering guarantees against the
// entrypoint. Both handlers retry briefly, because the exec path needs
// the container's status published and an HTTP server a moment to
// listen. Failures are logged and stop nothing.
func (s *supervisor) runPostStart(c *ctr) {
	h := c.spec.Lifecycle.PostStart
	if h == nil {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		switch {
		case h.Exec != nil:
			cmd := exec.Command(Bin, append([]string{"exec", "-c", c.spec.Name, "--"}, h.Exec...)...)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			err = cmd.Run()
		case h.HTTP != nil:
			g := h.HTTP
			var req *http.Request
			req, err = http.NewRequest(http.MethodGet, g.Scheme+"://"+net.JoinHostPort(g.Host, strconv.Itoa(g.Port))+g.Path, nil)
			if err == nil {
				cx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				var resp *http.Response
				resp, err = http.DefaultClient.Do(req.WithContext(cx))
				cancel()
				if err == nil {
					resp.Body.Close()
					return
				}
			}
		default:
			return
		}
		if err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	log.Printf("postStart %s: gave up", c.spec.Name)
}

// runPreStop runs the container's preStop handler and returns when it is
// done or the deadline passed, whichever comes first. A handler error is
// logged and stops nothing: the kubelet treats preStop failures the same
// way.
func (s *supervisor) runPreStop(c *ctr, deadline time.Time) {
	h := c.spec.Lifecycle
	if h == nil || h.PreStop == nil {
		return
	}
	left := time.Until(deadline)
	if left <= 0 {
		return
	}
	switch {
	case h.PreStop.Exec != nil:
		cmd := exec.Command(Bin, append([]string{"exec", "-c", c.spec.Name, "--"}, h.PreStop.Exec...)...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			log.Printf("preStop %s: %v", c.spec.Name, err)
			return
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(left):
			_ = cmd.Process.Kill()
			<-done
			log.Printf("preStop %s: grace period over", c.spec.Name)
		}
	case h.PreStop.HTTP != nil:
		g := h.PreStop.HTTP
		req, err := http.NewRequest(http.MethodGet, g.Scheme+"://"+net.JoinHostPort(g.Host, strconv.Itoa(g.Port))+g.Path, nil)
		if err == nil {
			cx, cancel := context.WithTimeout(context.Background(), left)
			resp, derr := http.DefaultClient.Do(req.WithContext(cx))
			if derr == nil {
				resp.Body.Close()
			}
			cancel()
			err = derr
		}
		if err != nil {
			log.Printf("preStop %s: %v", c.spec.Name, err)
		}
	}
}

func (s *supervisor) beginStop(grace time.Duration, result string) {
	if s.stopping {
		if t := time.Now().Add(grace); t.Before(s.stopAt) {
			s.stopAt = t
		}
		return
	}
	s.stopping = true
	s.result = result
	s.stopAt = time.Now().Add(grace)
	for _, c := range s.ctrs {
		if !c.running() && !c.done && c.term != nil {
			// A container in back-off stays terminated.
			c.done = true
		}
	}
}

func (s *supervisor) start(c *ctr) {
	if c.runs > 0 {
		c.restarts++
		c.last = c.term
	}
	c.runs++
	c.term, c.startErr, c.waitMsg = nil, "", ""
	c.termSent, c.killAt = false, time.Time{}
	pid, logs, err := startContainer(s.pod, c.spec)
	c.logs = logs
	c.startedAt = time.Now()
	if err != nil {
		log.Printf("container %s: start: %v", c.spec.Name, err)
		c.startErr = err.Error()
		if pid == 0 {
			s.ended(c, 128, 0, time.Now())
			return
		}
	}
	c.pid = pid
	log.Printf("container %s: started pid %d (run %d)", c.spec.Name, pid, c.runs)
	if c.spec.Lifecycle != nil && c.spec.Lifecycle.PostStart != nil {
		go s.runPostStart(c)
	}
}

func (s *supervisor) onExit(ev exitEvent) {
	for _, c := range s.ctrs {
		if c.pid != ev.pid {
			continue
		}
		code, sig := int32(ev.ws.ExitStatus()), int32(0)
		if ev.ws.Signaled() {
			sig = int32(ev.ws.Signal())
			code = 128 + sig
		}
		if c.startErr != "" {
			code = 128
		}
		c.pid = 0
		log.Printf("container %s: pid %d exited with %d", c.spec.Name, ev.pid, code)
		s.ended(c, code, sig, ev.at)
		return
	}
}

// ended records the end of a run and decides what follows: done, a
// restart now or after a back-off, or a failed pod.
func (s *supervisor) ended(c *ctr, code, sig int32, at time.Time) {
	t := &Termination{ExitCode: code, Signal: sig, Reason: "Completed", StartedAt: c.startedAt, FinishedAt: at}
	switch {
	case c.startErr != "":
		t.Reason, t.Message = "StartError", c.startErr
	case code != 0:
		t.Reason = "Error"
	}
	if t.Message == "" {
		t.Message = terminationMessage(c, code)
	}
	c.term = t
	c.killAt = time.Time{}
	if s.stopping {
		c.done = true
		return
	}
	restart := false
	switch {
	case c.spec.Init && c.spec.RestartAlways:
		restart = true
	case c.spec.Init:
		if code == 0 {
			c.done = true
			return
		}
		if s.pod.RestartPolicy == RestartNever {
			c.done = true
			s.message = fmt.Sprintf("init container %s failed with exit code %d", c.spec.Name, code)
			s.beginStop(time.Duration(s.grace())*time.Second, PhaseFailed)
			return
		}
		restart = true
	default:
		restart = s.pod.RestartPolicy == RestartAlways ||
			(s.pod.RestartPolicy == RestartOnFailure && code != 0)
	}
	if !restart {
		c.done = true
		return
	}
	if at.Sub(c.startedAt) >= backoffReset {
		c.backoff = 0
	}
	delay := c.backoff
	c.backoff = min(max(c.backoff*2, backoffFirst), backoffMax)
	c.nextStart = at.Add(delay)
	if delay > 0 {
		c.waitMsg = fmt.Sprintf("back-off %s restarting failed container=%s pod=%s_%s(%s)",
			delay, c.spec.Name, s.pod.Name, s.pod.Namespace, s.pod.UID)
	}
}

// terminationMessage reads the container's termination log, or the end of
// its output when the policy falls back to logs and the run failed.
func terminationMessage(c *ctr, code int32) string {
	const maxMsg = 4096
	if p := c.spec.TerminationLog; p != "" {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			if len(b) > maxMsg {
				b = b[:maxMsg]
			}
			return string(b)
		}
	}
	if c.spec.FallbackToLogs && code != 0 && c.logs != nil {
		return c.logs.Tail(80, 2048)
	}
	return ""
}

func (s *supervisor) onCtl(req CtlRequest) CtlResponse {
	fail := func(f string, a ...any) CtlResponse { return CtlResponse{Error: fmt.Sprintf(f, a...)} }
	switch req.Op {
	case CtlStop:
		log.Printf("ctl: stop with %d s grace", req.Grace)
		s.beginStop(time.Duration(req.Grace)*time.Second, "")
		return CtlResponse{OK: true}
	case CtlRestart, CtlKill:
		c := s.find(req.Container)
		if c == nil {
			return fail("no container %q", req.Container)
		}
		if !c.running() {
			return fail("container %s is not running", req.Container)
		}
		if req.Op == CtlKill {
			if err := syscall.Kill(c.pid, syscall.Signal(req.Signal)); err != nil {
				return fail("kill %s: %v", req.Container, err)
			}
			return CtlResponse{OK: true}
		}
		log.Printf("ctl: restart %s with %d s grace", req.Container, req.Grace)
		s.signalStop(c, time.Now().Add(time.Duration(req.Grace)*time.Second))
		return CtlResponse{OK: true}
	}
	return fail("unknown op %q", req.Op)
}

func (s *supervisor) phase() string {
	switch {
	case s.final && s.result != "":
		return s.result
	case s.stopping:
		return PhaseStopping
	case !s.initialized:
		return PhaseInitializing
	}
	return PhaseRunning
}

func (s *supervisor) status() *Status {
	st := &Status{Boot: s.boot, StartedAt: s.started, Phase: s.phase(), Initialized: s.initialized,
		Message: s.message, Final: s.final}
	reachedInit := true
	for _, c := range s.ctrs {
		cs := ContainerStatus{Name: c.spec.Name, RestartCount: c.restarts, Last: c.last}
		switch {
		case c.running():
			cs.State, cs.PID, cs.StartedAt = StateRunning, c.pid, c.startedAt
		case c.term != nil && (c.done || s.stopping):
			cs.State, cs.Term = StateTerminated, c.term
		case c.term != nil:
			cs.State, cs.Last = StateWaiting, c.term
			if c.waitMsg != "" {
				cs.Reason, cs.Message = "CrashLoopBackOff", c.waitMsg
			}
		default:
			cs.State, cs.Reason = StateWaiting, "PodInitializing"
			if s.initialized || (c.spec.Init && reachedInit) {
				cs.Reason = "ContainerCreating"
			}
		}
		if c.spec.Init && !c.spec.RestartAlways && !c.done {
			reachedInit = false
		}
		st.Containers = append(st.Containers, cs)
	}
	return st
}

// writeStatus replaces status.json when the status changed.
func (s *supervisor) writeStatus() {
	st := s.status()
	b, _ := json.Marshal(st)
	if bytes.Equal(b, s.lastStatus) {
		return
	}
	s.lastStatus = b
	st.UpdatedAt = time.Now()
	out, _ := json.MarshalIndent(st, "", "  ")
	tmp := StatusFile + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		log.Printf("status: %v", err)
		return
	}
	if err := os.Rename(tmp, StatusFile); err != nil {
		log.Printf("status: %v", err)
	}
}

func (s *supervisor) drainLogs(d time.Duration) {
	deadline := time.Now().Add(d)
	for _, c := range s.ctrs {
		if c.logs != nil {
			c.logs.Wait(time.Until(deadline))
		}
	}
}

// logPath is where a container's current log lives.
func logPath(name string) string { return filepath.Join(LogDir, name+".log") }
