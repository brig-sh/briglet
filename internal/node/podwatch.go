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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/brig-sh/briglet/internal/guest"
	"github.com/brig-sh/briglet/internal/hull"
)

// How a VM ended, as watch saw it.
type vmEnd int

const (
	vmRunning vmEnd = iota // watch returned for another reason
	vmStopped              // the VMM exited: the guest powered off or crashed
	vmRemoved              // the instance is gone: briglet down, or by hand
)

// watch follows the running VM: it reads status.json every guestPoll,
// checks that the VMM lives every pollInterval, runs the probes when they
// are due and keeps the volumes current. It returns how the VM ended, or
// vmRunning when the pod started stopping or ctx ended.
func (w *podWorker) watch(ctx context.Context) vmEnd {
	nextInspect := time.Now()
	for {
		if w.stopping() || ctx.Err() != nil {
			return vmRunning
		}
		// A pod past its deadline goes back to the loop, which stops it.
		if w.deadlinePassed() {
			return vmRunning
		}
		pod, _ := w.current()
		pod = w.effectivePod(ctx, pod)
		w.readGuest(ctx, pod)
		if now := time.Now(); !now.Before(nextInspect) {
			id := w.snapshot().Instance
			in, err := w.m.hull.Inspect(ctx, id)
			switch {
			case errors.Is(err, hull.ErrNotFound):
				return vmRemoved
			case err != nil:
				w.logf("inspect %s: %v", id, err)
			case !in.Alive():
				w.readGuest(ctx, pod)
				return vmStopped
			}
			nextInspect = now.Add(pollInterval)
			w.gcVolumes(pod)
			if st := w.snapshot(); st.Guest == nil && now.Sub(st.StartedAt) > supervisorWait {
				w.supervisorFailed(ctx, fmt.Sprintf("briglet-guest wrote no status.json within %s of boot", supervisorWait))
				return vmStopped
			}
		}
		w.maybeRefreshVolumes(ctx, pod)
		changed, next := w.probe(ctx, pod, time.Now())
		if changed {
			w.syncStatus(ctx)
		}
		wait := min(guestPoll, time.Until(nextInspect))
		if !next.IsZero() {
			wait = min(wait, time.Until(next))
		}
		if tr := w.snapshot().TokenRefreshAt; !tr.IsZero() {
			wait = min(wait, time.Until(tr))
		}
		if w.nap(ctx, max(wait, 50*time.Millisecond)) {
			return vmRunning
		}
	}
}

// readGuest reads status.json and, when it changed, records it, announces
// new container runs and back-offs with events, and writes the pod status.
func (w *podWorker) readGuest(ctx context.Context, pod *corev1.Pod) {
	b, err := os.ReadFile(filepath.Join(w.podDirs().Guest, "status.json"))
	if err != nil {
		return
	}
	var gs guest.Status
	if err := json.Unmarshal(b, &gs); err != nil {
		return
	}
	prev := w.snapshot().Guest
	if prev != nil && prev.Boot == gs.Boot && prev.UpdatedAt.Equal(gs.UpdatedAt) {
		return
	}
	var events []func()
	w.set(func(st *podState) {
		st.Guest = &gs
		for _, cs := range gs.Containers {
			c := containerOf(pod, cs.Name)
			if c == nil {
				continue
			}
			cst := st.ctr(cs.Name)
			if cs.State == guest.StateRunning && !cst.Run.Equal(cs.StartedAt) {
				*cst = ctrState{Run: cs.StartedAt, BackOff: cst.BackOff}
				initialProbeState(c, cst)
				name := cs.Name
				events = append(events, func() {
					w.event(corev1.EventTypeNormal, "Created", "Created container: %s", name)
					w.event(corev1.EventTypeNormal, "Started", "Started container %s", name)
				})
			}
			if cs.Reason == "CrashLoopBackOff" && cs.Last != nil && !cst.BackOff.Equal(cs.Last.FinishedAt) {
				cst.BackOff = cs.Last.FinishedAt
				name := cs.Name
				events = append(events, func() {
					w.event(corev1.EventTypeWarning, "BackOff", "Back-off restarting failed container %s in pod %s_%s(%s)", name, w.name, w.ns, w.uid)
				})
			}
		}
	})
	for _, e := range events {
		e()
	}
	if gs.Final {
		w.logf("guest: pod %s %s", gs.Phase, gs.Message)
	}
	w.syncStatus(ctx)
}

// containerOf finds a container of the pod by name.
func containerOf(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

// vmEnded decides what follows the end of the VM. A pod the guest ended
// is done, and so is one whose VM powered off before the supervisor ran. A
// VM that went away under running containers (a crash, `briglet down`)
// ends the pod Failed under restartPolicy Never and boots again otherwise,
// the containers that ran counting one restart. It reports whether the pod
// is done.
func (w *podWorker) vmEnded(ctx context.Context, how vmEnd) bool {
	pod, _ := w.current()
	st := w.snapshot()
	switch {
	case st.Done:
		w.set(func(st *podState) { st.Running = false })
		return true
	case st.Guest.Done():
		w.logf("VM %s ended; pod %s", st.Instance, st.Guest.Phase)
		w.set(func(st *podState) { st.Running = false; st.Done = true; st.Phase = st.Guest.Phase })
		return true
	case st.Guest == nil && how == vmStopped:
		w.supervisorFailed(ctx, "the VM ended before briglet-guest wrote status.json")
		w.set(func(st *podState) { st.Running = false })
		return true
	}
	now := time.Now()
	never := pod.Spec.RestartPolicy == corev1.RestartPolicyNever
	lost := guest.Status{Phase: guest.PhaseStopping}
	if st.Guest != nil {
		lost = *st.Guest
	}
	lost.Containers = nil
	restarts := cloneMap(st.Restarts)
	if restarts == nil {
		restarts = map[string]int32{}
	}
	var prev []guest.ContainerStatus
	if st.Guest != nil {
		prev = st.Guest.Containers
	}
	for _, cs := range prev {
		restarts[cs.Name] = cs.RestartCount
		if cs.State == guest.StateRunning {
			t := &guest.Termination{ExitCode: exitCodeKilled, Reason: "Error", Message: "the pod's VM stopped",
				StartedAt: cs.StartedAt, FinishedAt: now}
			cs.State, cs.PID, cs.StartedAt = guest.StateWaiting, 0, time.Time{}
			if never {
				cs.State, cs.Term = guest.StateTerminated, t
			} else {
				cs.Last = t
				cs.Reason, cs.Message = "CrashLoopBackOff", "the pod's VM stopped; booting it again"
				restarts[cs.Name]++
			}
		}
		lost.Containers = append(lost.Containers, cs)
	}
	lost.Final = true
	w.logf("VM %s ended before the pod did", st.Instance)
	w.set(func(st *podState) {
		st.Running = false
		st.Guest = &lost
		st.Restarts = restarts
		if never {
			st.Done, st.Phase = true, string(corev1.PodFailed)
		}
	})
	if !never {
		w.event(corev1.EventTypeWarning, "BackOff", "The VM of pod %s_%s(%s) stopped; booting it again", w.name, w.ns, w.uid)
	}
	return never
}

// supervisorFailed ends the pod Failed when briglet-guest did not run:
// the VM is stopped and the console's last lines go into the message.
func (w *podWorker) supervisorFailed(ctx context.Context, why string) {
	id := w.snapshot().Instance
	var buf bytes.Buffer
	_ = w.m.hull.Logs(ctx, id, false, 20, &buf)
	var tail []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if !kernelLine.MatchString(line) && strings.TrimSpace(line) != "" {
			tail = append(tail, line)
		}
	}
	msg := why
	if len(tail) > 0 {
		msg += "; console: " + strings.Join(tail[max(0, len(tail)-5):], " | ")
	}
	w.logf("%s", msg)
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	_ = w.m.hull.Stop(sctx, id, 2)
	cancel()
	w.event(corev1.EventTypeWarning, "Failed", "%s", msg)
	w.set(func(st *podState) {
		st.Done, st.Phase = true, string(corev1.PodFailed)
		st.Reason, st.Message = "GuestSupervisorFailed", msg
	})
}

// ctl sends a request to the pod's supervisor through the guest agent.
func (w *podWorker) ctl(ctx context.Context, req guest.CtlRequest) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	sess, err := hull.DialAgent(ctx, w.m.hull.AgentSocket(w.snapshot().Instance),
		hull.OpenRequest{Argv: []string{guest.Bin, "ctl", string(b)}})
	if err != nil {
		return err
	}
	defer sess.Close()
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()
	_ = sess.CopyStdin(strings.NewReader(""))
	out := &limitedBuffer{max: 4096}
	code, err := sess.Wait(out, out)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("ctl %s: exit %d: %s", req.Op, code, strings.TrimSpace(out.String()))
	}
	return nil
}

// guestExec prefixes argv so the guest agent runs it inside container.
func guestExec(container string, argv []string) []string {
	return append([]string{guest.Bin, "exec", "-c", container, "--"}, argv...)
}
