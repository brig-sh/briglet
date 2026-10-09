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

package podvm

import (
	"fmt"
	"math"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// FieldValue resolves a downward API fieldRef path for the pod.
func FieldValue(pod *corev1.Pod, f PodFacts, path string) (string, error) {
	switch path {
	case "metadata.name":
		return pod.Name, nil
	case "metadata.namespace":
		return pod.Namespace, nil
	case "metadata.uid":
		return string(pod.UID), nil
	case "metadata.labels":
		return formatMap(pod.Labels), nil
	case "metadata.annotations":
		return formatMap(pod.Annotations), nil
	case "spec.nodeName":
		return f.NodeName, nil
	case "spec.serviceAccountName":
		return pod.Spec.ServiceAccountName, nil
	case "status.hostIP", "status.hostIPs":
		return f.HostIP, nil
	case "status.podIP", "status.podIPs":
		return f.PodIP, nil
	}
	if k, ok := subscript(path, "metadata.labels"); ok {
		return pod.Labels[k], nil
	}
	if k, ok := subscript(path, "metadata.annotations"); ok {
		return pod.Annotations[k], nil
	}
	return "", fmt.Errorf("fieldRef %s is not supported", path)
}

// subscript parses prefix['key'].
func subscript(path, prefix string) (string, bool) {
	rest, ok := strings.CutPrefix(path, prefix+"['")
	if !ok || !strings.HasSuffix(rest, "']") {
		return "", false
	}
	return strings.TrimSuffix(rest, "']"), true
}

// formatMap renders labels or annotations as the kubelet does: sorted
// key="value" lines, the value quoted with Go syntax.
func formatMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%v=%q\n", k, m[k])
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// ResourceValue resolves a resourceFieldRef against the container. A limit
// the container does not set is the size of its VM, where the kubelet would
// use the node allocatable: the VM is the bound that holds. An unset request
// is 0.
func ResourceValue(fs *corev1.ResourceFieldSelector, c *corev1.Container, f PodFacts) (string, error) {
	divisor := fs.Divisor
	if divisor.IsZero() {
		divisor = resource.MustParse("1")
	}
	get := func(rl corev1.ResourceList, name corev1.ResourceName) (resource.Quantity, bool) {
		q, ok := rl[name]
		return q, ok && !q.IsZero()
	}
	switch fs.Resource {
	case "limits.cpu":
		q, ok := get(c.Resources.Limits, corev1.ResourceCPU)
		if !ok {
			q = *resource.NewQuantity(int64(f.CPUs), resource.DecimalSI)
		}
		return cpuString(q, divisor), nil
	case "requests.cpu":
		q, _ := get(c.Resources.Requests, corev1.ResourceCPU)
		return cpuString(q, divisor), nil
	case "limits.memory":
		q, ok := get(c.Resources.Limits, corev1.ResourceMemory)
		if !ok {
			q = *resource.NewQuantity(int64(f.MemMB)<<20, resource.BinarySI)
		}
		return valueString(q, divisor), nil
	case "requests.memory":
		q, _ := get(c.Resources.Requests, corev1.ResourceMemory)
		return valueString(q, divisor), nil
	case "limits.ephemeral-storage":
		q, _ := get(c.Resources.Limits, corev1.ResourceEphemeralStorage)
		return valueString(q, divisor), nil
	case "requests.ephemeral-storage":
		q, _ := get(c.Resources.Requests, corev1.ResourceEphemeralStorage)
		return valueString(q, divisor), nil
	}
	return "", fmt.Errorf("resourceFieldRef %s is not supported", fs.Resource)
}

// cpuString and valueString round up after dividing, as the kubelet does.
func cpuString(q, divisor resource.Quantity) string {
	return fmt.Sprint(int64(math.Ceil(float64(q.MilliValue()) / float64(divisor.MilliValue()))))
}

func valueString(q, divisor resource.Quantity) string {
	return fmt.Sprint(int64(math.Ceil(float64(q.Value()) / float64(divisor.Value()))))
}
