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
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestProbeDue(t *testing.T) {
	start := time.Unix(1000, 0)
	ps := probeSpec{probeLiveness, &corev1.Probe{InitialDelaySeconds: 5, PeriodSeconds: 7}}
	st := &podState{StartedAt: start}
	if got := probeDue(ps, st); !got.Equal(start.Add(5 * time.Second)) {
		t.Errorf("first due = %v", got)
	}
	st.Probes = map[string]*probeState{probeLiveness: {LastAt: start.Add(6 * time.Second)}}
	if got := probeDue(ps, st); !got.Equal(start.Add(13 * time.Second)) {
		t.Errorf("next due = %v", got)
	}
	// A result from before the container started does not count.
	st.Probes[probeLiveness].LastAt = start.Add(-time.Second)
	if got := probeDue(ps, st); !got.Equal(start.Add(5 * time.Second)) {
		t.Errorf("due after restart = %v", got)
	}
}

func TestInitialProbeState(t *testing.T) {
	c := &corev1.Container{}
	st := &podState{Probes: map[string]*probeState{probeLiveness: {Failures: 2}}}
	initialProbeState(c, st)
	if !st.Started || !st.Ready || st.Probes != nil {
		t.Errorf("no probes: %+v", st)
	}
	c.ReadinessProbe = &corev1.Probe{}
	initialProbeState(c, st)
	if !st.Started || st.Ready {
		t.Errorf("readiness probe: %+v", st)
	}
	c.StartupProbe = &corev1.Probe{}
	initialProbeState(c, st)
	if st.Started || st.Ready {
		t.Errorf("startup probe: %+v", st)
	}
}

func TestProbePorts(t *testing.T) {
	c := &corev1.Container{
		Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: 80}, {ContainerPort: 53, Protocol: corev1.ProtocolUDP}},
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromString("http")}}},
		LivenessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8080)}}},
	}
	got := probePorts(c)
	if len(got) != 2 || got[0] != 80 || got[1] != 8080 {
		t.Errorf("probePorts = %v", got)
	}
	if _, err := resolvePort(intstr.FromString("metrics"), c); err == nil {
		t.Error("unknown port name resolved")
	}
}
