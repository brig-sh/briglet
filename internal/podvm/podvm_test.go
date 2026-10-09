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
	"path/filepath"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/brig-sh/briglet/internal/hull"
)

func TestArgv(t *testing.T) {
	img := &hull.ImageConfig{Entrypoint: []string{"/docker-entrypoint.sh"}, Cmd: []string{"nginx", "-g", "daemon off;"}}
	env := []string{"NAME=web"}
	cases := []struct {
		c    corev1.Container
		want []string
	}{
		{corev1.Container{}, []string{"/docker-entrypoint.sh", "nginx", "-g", "daemon off;"}},
		{corev1.Container{Args: []string{"nginx", "-T"}}, []string{"/docker-entrypoint.sh", "nginx", "-T"}},
		{corev1.Container{Command: []string{"sh", "-c"}, Args: []string{"echo $(NAME) $$(NAME) $(NOPE)"}},
			[]string{"sh", "-c", "echo web $(NAME) $(NOPE)"}},
		{corev1.Container{Command: []string{"sleep"}}, []string{"sleep"}},
	}
	for _, tc := range cases {
		got, err := Argv(&tc.c, img, env)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Argv(%+v) = %q, %v; want %q", tc.c, got, err, tc.want)
		}
	}
	if _, err := Argv(&corev1.Container{}, &hull.ImageConfig{}, nil); err == nil {
		t.Error("Argv with no command anywhere: want an error")
	}
}

func TestResources(t *testing.T) {
	c := &corev1.Container{}
	if cpu, mem := Resources(c); cpu != 1 || mem != 512 {
		t.Errorf("defaults = %d, %d", cpu, mem)
	}
	c.Resources.Limits = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("1500m"),
		corev1.ResourceMemory: resource.MustParse("300M"),
	}
	if cpu, mem := Resources(c); cpu != 2 || mem != 287 {
		t.Errorf("limits = %d, %d; want 2, 287", cpu, mem)
	}
}

func TestEnv(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns", UID: "u-1"}}
	c := &corev1.Container{Env: []corev1.EnvVar{
		{Name: "PATH", Value: "/x:$(PATH)"},
		{Name: "IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "NS", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
	}}
	got, err := Env(pod, c, &hull.ImageConfig{Env: []string{"PATH=/bin"}}, FieldValues{PodIP: "10.87.0.10"})
	want := []string{"HOSTNAME=web", "PATH=/x:/bin", "IP=10.87.0.10", "NS=ns"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Env = %q, %v; want %q", got, err, want)
	}
	c.Env = []corev1.EnvVar{{Name: "S", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "k"}}}}
	if _, err := Env(pod, c, &hull.ImageConfig{}, FieldValues{}); err == nil {
		t.Error("secretKeyRef: want an error")
	}
}

func TestInstanceName(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "1234abcd-5678"}}
	if got := InstanceName(pod, 48); got != "default_web_1234abcd" {
		t.Errorf("InstanceName = %q", got)
	}
	pod.Name = "a-very-long-pod-name-that-keeps-going-and-going"
	got := InstanceName(pod, 40)
	if len(got) > 40 || got[len(got)-9:] != "_1234abcd" {
		t.Errorf("long InstanceName = %q (%d)", got, len(got))
	}
}

func TestValidate(t *testing.T) {
	ok := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}}}
	if r := Validate(ok); r != "" {
		t.Errorf("Validate(ok) = %q", r)
	}
	bad := ok.DeepCopy()
	bad.Spec.HostNetwork = true
	bad.Spec.Containers = append(bad.Spec.Containers, corev1.Container{Name: "d"})
	if r := Validate(bad); r == "" {
		t.Error("Validate(hostNetwork, two containers) passed")
	}
}

func TestIPAM(t *testing.T) {
	m, err := NewIPAM(filepath.Join(t.TempDir(), "ipam.json"), "10.87.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := m.Allocate("a", "ns/a")
	b, _ := m.Allocate("b", "ns/b")
	a2, _ := m.Allocate("a", "ns/a")
	if a != "10.87.0.10" || b != "10.87.0.11" || a2 != a {
		t.Fatalf("allocations a=%s b=%s a again=%s", a, b, a2)
	}
	if err := m.Release("a"); err != nil {
		t.Fatal(err)
	}
	// A released address comes back only once the range wraps.
	if c, _ := m.Allocate("c", "ns/c"); c != "10.87.0.12" {
		t.Errorf("allocation after a release = %s, want the next address", c)
	}
	gone, err := m.Retain(map[string]bool{"c": true})
	if err != nil || len(gone) != 1 || gone[0].UID != "b" {
		t.Errorf("Retain released %v, %v", gone, err)
	}
	for i := 13; i <= 254; i++ {
		if _, err := m.Allocate(fmt.Sprint("fill", i), "ns/fill"); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	if w, _ := m.Allocate("w", "ns/w"); w != "10.87.0.10" {
		t.Errorf("allocation after the wrap = %s, want 10.87.0.10", w)
	}
	if x, _ := m.Allocate("x", "ns/x"); x != "10.87.0.11" {
		t.Errorf("second allocation after the wrap = %s, want 10.87.0.11", x)
	}
	if _, err := m.Allocate("y", "ns/y"); err == nil {
		t.Error("allocation from a full range succeeded")
	}
}
