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

// Package podvm maps a Pod to the hull run arguments of its microVM.
package podvm

import (
	"fmt"
	"math"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/brig-sh/briglet/internal/hull"
)

// Defaults for a container without CPU or memory limits.
const (
	DefaultCPUs  = 1
	DefaultMemMB = 512
)

// ReasonUnsupported is the event and status reason for a rejected pod.
const ReasonUnsupported = "Unsupported"

// Validate returns why the pod cannot run on briglet, or "" when it can.
func Validate(pod *corev1.Pod) string {
	s := &pod.Spec
	var why []string
	if s.HostNetwork {
		why = append(why, "hostNetwork")
	}
	if s.HostPID {
		why = append(why, "hostPID")
	}
	if s.HostIPC {
		why = append(why, "hostIPC")
	}
	if len(s.InitContainers) > 0 {
		why = append(why, "init containers")
	}
	if len(s.Containers) != 1 {
		why = append(why, fmt.Sprintf("%d containers (one is supported)", len(s.Containers)))
	}
	if len(s.EphemeralContainers) > 0 {
		why = append(why, "ephemeral containers")
	}
	if sc := s.SecurityContext; sc != nil && sc.RunAsUser != nil {
		why = append(why, "securityContext.runAsUser")
	}
	for _, c := range s.Containers {
		if sc := c.SecurityContext; sc != nil && sc.RunAsUser != nil {
			why = append(why, "container securityContext.runAsUser")
		}
	}
	why = append(why, unsupportedVolumes(pod)...)
	if len(why) == 0 {
		return ""
	}
	return "briglet does not support " + strings.Join(why, ", ")
}

// Ignored lists container settings briglet accepts but does not apply.
func Ignored(c *corev1.Container) []string {
	var out []string
	if c.WorkingDir != "" {
		out = append(out, fmt.Sprintf("workingDir %q is ignored: hull starts the container in /", c.WorkingDir))
	}
	for _, p := range []*corev1.Probe{c.StartupProbe, c.LivenessProbe, c.ReadinessProbe} {
		if p != nil && p.GRPC != nil {
			out = append(out, fmt.Sprintf("gRPC probe on port %d is checked as a TCP connect: briglet does not speak the gRPC health protocol", p.GRPC.Port))
			break
		}
	}
	return out
}

// InstanceName is the hull instance name of a pod: <ns>_<pod>_<uid8>. The
// agent socket path under the store must fit in a sockaddr_un, so the
// namespace and name part is cut to fit maxLen.
func InstanceName(pod *corev1.Pod, maxLen int) string {
	uid := strings.ReplaceAll(string(pod.UID), "-", "")
	if len(uid) > 8 {
		uid = uid[:8]
	}
	head := pod.Namespace + "_" + pod.Name
	if maxLen < 16 {
		maxLen = 16
	}
	if room := maxLen - len(uid) - 1; len(head) > room {
		head = strings.TrimRight(head[:room], "._-")
	}
	return head + "_" + uid
}

// Resources returns vCPUs and memory in MB from the container limits. CPU is
// rounded up to whole vCPUs, memory up to whole MiB.
func Resources(c *corev1.Container) (cpus, memMB int) {
	cpus, memMB = DefaultCPUs, DefaultMemMB
	if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok && !q.IsZero() {
		cpus = int(math.Ceil(float64(q.MilliValue()) / 1000))
	}
	if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok && !q.IsZero() {
		memMB = int(math.Ceil(float64(q.Value()) / (1 << 20)))
	}
	return cpus, memMB
}

// PullPolicy maps imagePullPolicy to hull's --pull values.
func PullPolicy(c *corev1.Container) string {
	switch c.ImagePullPolicy {
	case corev1.PullAlways:
		return "always"
	case corev1.PullNever:
		return "never"
	}
	return "missing"
}

// Argv computes the container's full argv with Kubernetes semantics:
// command replaces the image Entrypoint, args replace the image Cmd, and
// $(VAR) references expand from env.
func Argv(c *corev1.Container, img *hull.ImageConfig, env []string) ([]string, error) {
	var argv []string
	switch {
	case len(c.Command) > 0:
		argv = append(append(argv, c.Command...), c.Args...)
	case len(c.Args) > 0:
		argv = append(append(argv, img.Entrypoint...), c.Args...)
	default:
		argv = append(append(argv, img.Entrypoint...), img.Cmd...)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("container %s: no command and the image sets no Entrypoint or Cmd", c.Name)
	}
	vars := envMap(env)
	// Only command and args expand; the image's own values are literal.
	if len(c.Command) > 0 || len(c.Args) > 0 {
		for i, a := range argv {
			argv[i] = expand(a, vars)
		}
	}
	return argv, nil
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// expand replaces $(VAR) with its value from vars and $$ with $, as the
// kubelet does. An undefined reference stays as written.
func expand(s string, vars map[string]string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '(':
			end := strings.IndexByte(s[i+2:], ')')
			if end < 0 {
				b.WriteString(s[i:])
				return b.String()
			}
			name := s[i+2 : i+2+end]
			if v, ok := vars[name]; ok {
				b.WriteString(v)
			} else {
				b.WriteString(s[i : i+3+end])
			}
			i += 2 + end
		default:
			b.WriteByte('$')
		}
	}
	return b.String()
}

// GraceSeconds is the termination grace period of a pod.
func GraceSeconds(pod *corev1.Pod) int {
	if pod.DeletionGracePeriodSeconds != nil {
		return int(*pod.DeletionGracePeriodSeconds)
	}
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		return int(*pod.Spec.TerminationGracePeriodSeconds)
	}
	return 30
}
