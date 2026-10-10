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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/brig-sh/briglet/internal/guest"
)

func TestStatusFromGuest(t *testing.T) {
	now := time.Unix(2000, 0)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "u"}, Spec: corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		InitContainers: []corev1.Container{
			{Name: "init"},
			{Name: "side", RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways)},
		},
		Containers: []corev1.Container{{Name: "app"}},
	}}
	w := &podWorker{m: &podManager{hostIP: "10.87.0.1"}, pod: pod}

	// An init container failed under restartPolicy Never: Init:Error.
	w.st = podState{Instance: "ns_p_u", Created: true, IP: "10.87.0.20", Guest: &guest.Status{
		Phase: guest.PhaseFailed, Message: "init container init failed with exit code 3", Final: true,
		Containers: []guest.ContainerStatus{
			{Name: "init", State: guest.StateTerminated, Term: &guest.Termination{ExitCode: 3, Reason: "Error", FinishedAt: now}},
			{Name: "side", State: guest.StateWaiting, Reason: "PodInitializing"},
			{Name: "app", State: guest.StateWaiting, Reason: "PodInitializing"},
		},
	}}
	got := w.status(pod)
	if got.Phase != corev1.PodFailed || len(got.InitContainerStatuses) != 2 || len(got.ContainerStatuses) != 1 {
		t.Fatalf("phase %s, %d init, %d containers", got.Phase, len(got.InitContainerStatuses), len(got.ContainerStatuses))
	}
	if term := got.InitContainerStatuses[0].State.Terminated; term == nil || term.ExitCode != 3 || term.Reason != "Error" {
		t.Errorf("init status %+v", got.InitContainerStatuses[0].State)
	}
	if wt := got.ContainerStatuses[0].State.Waiting; wt == nil || wt.Reason != "PodInitializing" {
		t.Errorf("app status %+v", got.ContainerStatuses[0].State)
	}
	if got.Message == "" {
		t.Error("no message for the failed pod")
	}

	// Initialized, the sidecar and app running: Running, Ready once both
	// are ready.
	w.st.Guest = &guest.Status{Phase: guest.PhaseRunning, Initialized: true, Containers: []guest.ContainerStatus{
		{Name: "init", State: guest.StateTerminated, Term: &guest.Termination{Reason: "Completed", FinishedAt: now}},
		{Name: "side", State: guest.StateRunning, StartedAt: now, RestartCount: 1,
			Last: &guest.Termination{ExitCode: 137, Signal: 9, Reason: "Error", FinishedAt: now}},
		{Name: "app", State: guest.StateRunning, StartedAt: now},
	}}
	w.st.Running = true
	w.st.Containers = map[string]*ctrState{
		"side": {Run: now, Started: true, Ready: true},
		"app":  {Run: now, Started: true, Ready: false},
	}
	got = w.status(pod)
	cond := map[corev1.PodConditionType]corev1.ConditionStatus{}
	for _, c := range got.Conditions {
		cond[c.Type] = c.Status
	}
	if got.Phase != corev1.PodRunning || cond[corev1.PodInitialized] != corev1.ConditionTrue || cond[corev1.PodReady] != corev1.ConditionFalse {
		t.Errorf("phase %s conditions %v", got.Phase, cond)
	}
	side := got.InitContainerStatuses[1]
	if side.RestartCount != 1 || side.LastTerminationState.Terminated == nil || side.LastTerminationState.Terminated.ExitCode != 137 || side.ContainerID != "hull://ns_p_u/side/1" {
		t.Errorf("sidecar status %+v", side)
	}
	w.st.Containers["app"].Ready = true
	got = w.status(pod)
	for _, c := range got.Conditions {
		if c.Type == corev1.PodReady && c.Status != corev1.ConditionTrue {
			t.Errorf("Ready = %s with every container ready", c.Status)
		}
	}
}
