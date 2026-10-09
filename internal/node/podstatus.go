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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

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
			return nil
		}
		cur.Status = want
		_, err = pods.UpdateStatus(ctx, cur, metav1.UpdateOptions{})
		return err
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
	c := &cur.Spec.Containers[0]
	cs := corev1.ContainerStatus{
		Name:         c.Name,
		Image:        c.Image,
		ImageID:      st.ImageID,
		RestartCount: st.RestartCount,
		Started:      ptr.To(st.Running && st.Started),
		Ready:        st.Running && st.Ready,
	}
	if st.Created {
		cs.ContainerID = "hull://" + st.Instance
	}
	switch {
	case st.Running:
		cs.State.Running = &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(st.StartedAt)}
	case st.Waiting != nil:
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: st.Waiting.Reason, Message: st.Waiting.Message}
	case st.Term != nil:
		cs.State.Terminated = terminated(st.Term, cs.ContainerID)
	default:
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}
	}
	if st.LastTerm != nil {
		cs.LastTerminationState.Terminated = terminated(st.LastTerm, cs.ContainerID)
	}
	if st.Term != nil && cs.State.Waiting != nil {
		// Back-off: the last run is the most recent termination.
		cs.LastTerminationState.Terminated = terminated(st.Term, cs.ContainerID)
	}
	out.ContainerStatuses = []corev1.ContainerStatus{cs}

	switch {
	case st.Done && st.Term != nil && st.Term.ExitCode == 0:
		out.Phase = corev1.PodSucceeded
	case st.Done:
		out.Phase = corev1.PodFailed
	case st.Running || st.RestartCount > 0 || st.Term != nil:
		out.Phase = corev1.PodRunning
	default:
		out.Phase = corev1.PodPending
	}
	ready := cs.Ready
	out.Conditions = mergeConditions(cur, out.Conditions, map[corev1.PodConditionType]bool{
		corev1.PodScheduled:              true,
		corev1.PodReadyToStartContainers: st.Running || (st.Created && !st.Done),
		corev1.PodInitialized:            true,
		corev1.ContainersReady:           ready,
		corev1.PodReady:                  ready,
	})
	for i := range out.Conditions {
		cond := &out.Conditions[i]
		if cond.Status == corev1.ConditionTrue || (cond.Type != corev1.PodReady && cond.Type != corev1.ContainersReady) {
			continue
		}
		switch {
		case st.Done:
			cond.Reason = "PodCompleted"
		default:
			cond.Reason = "ContainersNotReady"
			cond.Message = "containers with unready status: [" + c.Name + "]"
		}
	}
	return out
}

func terminated(t *termState, id string) *corev1.ContainerStateTerminated {
	return &corev1.ContainerStateTerminated{
		ExitCode:    t.ExitCode,
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
