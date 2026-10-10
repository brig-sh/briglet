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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

	"github.com/brig-sh/briglet/internal/guest"
	"github.com/brig-sh/briglet/internal/podvm"
)

// syncStatus writes the pod status derived from the worker state. It reads
// the pod from the apiserver, so a stale informer copy cannot undo a write.
func (w *podWorker) syncStatus(ctx context.Context) {
	if _, gone := w.current(); gone {
		return
	}
	pods := w.m.cs.CoreV1().Pods(w.ns)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := pods.Get(ctx, w.name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if cur.UID != w.uid {
			return nil
		}
		want := w.status(cur)
		if equality.Semantic.DeepEqual(cur.Status, want) {
			w.obsGen.Store(cur.Generation)
			return nil
		}
		cur.Status = want
		if _, err = pods.UpdateStatus(ctx, cur, metav1.UpdateOptions{}); err != nil {
			return err
		}
		w.obsGen.Store(cur.Generation)
		return nil
	})
	if err != nil && !apierrors.IsNotFound(err) && ctx.Err() == nil {
		w.logf("update status: %v", err)
	}
}

// status computes the pod status. Fields the agent does not own, such as
// qosClass, are kept from cur.
func (w *podWorker) status(cur *corev1.Pod) corev1.PodStatus {
	st := w.snapshot()
	out := *cur.Status.DeepCopy()
	now := metav1.Now()
	if out.StartTime == nil {
		out.StartTime = &now
	}
	out.ObservedGeneration = cur.Generation
	out.HostIP = w.m.hostIP
	out.HostIPs = []corev1.HostIP{{IP: w.m.hostIP}}
	out.Reason, out.Message = "", ""

	if st.Rejected != "" {
		out.Phase = corev1.PodFailed
		out.Reason = podvm.ReasonUnsupported
		if st.RejectedReason != "" {
			out.Reason = st.RejectedReason
		}
		out.Message = st.Rejected
		out.Conditions = mergeConditions(cur, out.Conditions, map[corev1.PodConditionType]bool{
			corev1.PodScheduled: true,
		})
		return out
	}
	if st.Created {
		out.PodIP = st.IP
		out.PodIPs = []corev1.PodIP{{IP: st.IP}}
	}
	g := st.Guest
	hasInit := len(cur.Spec.InitContainers) > 0
	initialized := !hasInit || (g != nil && g.Initialized)
	out.InitContainerStatuses = nil
	out.ContainerStatuses = nil
	var notReady []string
	for i := range cur.Spec.InitContainers {
		c := &cur.Spec.InitContainers[i]
		cs := w.containerStatus(&st, c, hasInit, true)
		out.InitContainerStatuses = append(out.InitContainerStatuses, cs)
		if isSidecar(c) && !cs.Ready {
			notReady = append(notReady, c.Name)
		}
	}
	for i := range cur.Spec.Containers {
		c := &cur.Spec.Containers[i]
		cs := w.containerStatus(&st, c, hasInit, false)
		out.ContainerStatuses = append(out.ContainerStatuses, cs)
		if !cs.Ready {
			notReady = append(notReady, c.Name)
		}
	}

	switch {
	case st.Done && st.Phase != "":
		out.Phase = corev1.PodPhase(st.Phase)
		out.Reason, out.Message = st.Reason, st.Message
	case st.Deadline:
		out.Phase = corev1.PodFailed
		out.Reason = "DeadlineExceeded"
	case g != nil && g.Phase == guest.PhaseSucceeded:
		out.Phase = corev1.PodSucceeded
	case g != nil && g.Phase == guest.PhaseFailed:
		out.Phase = corev1.PodFailed
	case initialized && g != nil:
		out.Phase = corev1.PodRunning
	default:
		out.Phase = corev1.PodPending
	}
	if out.Phase == corev1.PodFailed && out.Message == "" && g != nil {
		out.Message = g.Message
	}
	ready := len(notReady) == 0
	out.Conditions = mergeConditions(cur, out.Conditions, map[corev1.PodConditionType]bool{
		corev1.PodScheduled:              true,
		corev1.PodReadyToStartContainers: st.Running,
		corev1.PodInitialized:            initialized,
		corev1.ContainersReady:           ready,
		corev1.PodReady:                  ready,
	})
	done := out.Phase == corev1.PodSucceeded || out.Phase == corev1.PodFailed
	for i := range out.Conditions {
		cond := &out.Conditions[i]
		cond.Reason, cond.Message = "", ""
		if cond.Status == corev1.ConditionTrue {
			continue
		}
		switch {
		case cond.Type == corev1.PodInitialized:
			cond.Reason = "ContainersNotInitialized"
			cond.Message = "containers with incomplete status: " + fmt.Sprint(incompleteInit(cur, g))
		case cond.Type != corev1.PodReady && cond.Type != corev1.ContainersReady:
		case done:
			cond.Reason = "PodCompleted"
		default:
			cond.Reason = "ContainersNotReady"
			cond.Message = "containers with unready status: " + fmt.Sprint(notReady)
		}
	}
	return out
}

func isSidecar(c *corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// incompleteInit names the init containers that have not completed (or,
// for sidecars, started).
func incompleteInit(pod *corev1.Pod, g *guest.Status) []string {
	var out []string
	for _, c := range pod.Spec.InitContainers {
		cs := g.Find(c.Name)
		switch {
		case cs == nil:
		case isSidecar(&c) && cs.State == guest.StateRunning:
			continue
		case cs.State == guest.StateTerminated && cs.Term != nil && cs.Term.ExitCode == 0:
			continue
		}
		out = append(out, c.Name)
	}
	return out
}

// containerStatus renders one container from the guest's status, or from
// the agent's own state while no VM reported yet.
func (w *podWorker) containerStatus(st *podState, c *corev1.Container, hasInit, init bool) corev1.ContainerStatus {
	cs := corev1.ContainerStatus{
		Name:         c.Name,
		Image:        c.Image,
		ImageID:      st.Images[c.Name],
		RestartCount: st.Restarts[c.Name],
		Started:      ptr.To(false),
	}
	g := st.Guest.Find(c.Name)
	id := func(restarts int32) string {
		return fmt.Sprintf("hull://%s/%s/%d", st.Instance, c.Name, restarts)
	}
	if g == nil && st.Done {
		cs.State.Terminated = &corev1.ContainerStateTerminated{ExitCode: exitCodeKilled, Reason: "ContainerStatusUnknown",
			Message: "The container could not be located when the pod was terminated"}
		return cs
	}
	if g == nil || (!st.Running && !st.Done && st.Waiting != nil) {
		reason, msg := "ContainerCreating", ""
		if hasInit {
			reason = "PodInitializing"
		}
		if wt := st.Waiting; wt != nil && (wt.Container == "" || wt.Container == c.Name) {
			reason, msg = wt.Reason, wt.Message
		}
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: reason, Message: msg}
		if g != nil && g.Last != nil {
			cs.LastTerminationState.Terminated = terminated(g.Last, id(g.RestartCount-1))
		}
		return cs
	}
	cs.RestartCount = g.RestartCount
	switch g.State {
	case guest.StateRunning:
		cs.ContainerID = id(g.RestartCount)
		cs.State.Running = &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(g.StartedAt)}
		if cst := st.Containers[c.Name]; cst != nil && cst.Run.Equal(g.StartedAt) {
			cs.Started = ptr.To(cst.Started)
			cs.Ready = cst.Ready
		}
	case guest.StateTerminated:
		cs.ContainerID = id(g.RestartCount)
		if g.Term != nil {
			cs.State.Terminated = terminated(g.Term, cs.ContainerID)
			// The kubelet reports a completed init container Ready.
			if init && g.Term.ExitCode == 0 {
				cs.Ready = true
			}
		}
		if st.Deadline && cs.State.Terminated != nil {
			cs.State.Terminated.Reason = "DeadlineExceeded"
		}
	default:
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: g.Reason, Message: g.Message}
		if g.Reason == "" {
			cs.State.Waiting.Reason = "ContainerCreating"
		}
	}
	if g.Last != nil {
		cs.LastTerminationState.Terminated = terminated(g.Last, id(max(g.RestartCount-1, 0)))
	}
	return cs
}

func terminated(t *guest.Termination, id string) *corev1.ContainerStateTerminated {
	return &corev1.ContainerStateTerminated{
		ExitCode:    t.ExitCode,
		Signal:      t.Signal,
		Reason:      t.Reason,
		Message:     t.Message,
		StartedAt:   metav1.NewTime(t.StartedAt),
		FinishedAt:  metav1.NewTime(t.FinishedAt),
		ContainerID: id,
	}
}

// mergeConditions sets the given conditions, keeping transition times of
// conditions whose status did not change and any condition not listed.
func mergeConditions(cur *corev1.Pod, have []corev1.PodCondition, want map[corev1.PodConditionType]bool) []corev1.PodCondition {
	now := metav1.Now()
	order := []corev1.PodConditionType{
		corev1.PodReadyToStartContainers, corev1.PodInitialized, corev1.PodReady,
		corev1.ContainersReady, corev1.PodScheduled,
	}
	byType := map[corev1.PodConditionType]corev1.PodCondition{}
	var extra []corev1.PodCondition
	for _, c := range have {
		if _, ok := want[c.Type]; ok {
			byType[c.Type] = c
		} else {
			extra = append(extra, c)
		}
	}
	var out []corev1.PodCondition
	for _, t := range order {
		v, ok := want[t]
		if !ok {
			continue
		}
		status := corev1.ConditionFalse
		if v {
			status = corev1.ConditionTrue
		}
		c := corev1.PodCondition{Type: t, Status: status, LastTransitionTime: now, ObservedGeneration: cur.Generation}
		if old, ok := byType[t]; ok {
			if old.Status == status {
				c.LastTransitionTime = old.LastTransitionTime
			}
			c.LastProbeTime = old.LastProbeTime
		}
		out = append(out, c)
	}
	return append(out, extra...)
}
