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
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/brig-sh/briglet/internal/hull"
)

func TestResolveUser(t *testing.T) {
	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(rootfs, "etc", "passwd"), []byte("root:x:0:0:root:/root:/bin/sh\nnginx:x:101:101:nginx:/var/cache/nginx:/sbin/nologin\nnonroot:x:65532:65532::/home/nonroot:/sbin/nologin\n"), 0o644)
	_ = os.WriteFile(filepath.Join(rootfs, "etc", "group"), []byte("root:x:0:\nnginx:x:101:\nwww:x:33:nginx\nnonroot:x:65532:\n"), 0o644)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	c := &corev1.Container{Name: "c"}
	cases := []struct {
		image string
		sc    *corev1.PodSecurityContext
		csc   *corev1.SecurityContext
		want  User
		err   string
	}{
		{image: "", want: User{0, 0, nil, "/root"}},
		{image: "nginx", want: User{101, 101, []uint32{33}, "/var/cache/nginx"}},
		{image: "nonroot:nonroot", want: User{65532, 65532, nil, "/home/nonroot"}},
		{image: "1234:55", want: User{1234, 55, nil, "/"}},
		{image: "nginx", sc: &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1000), SupplementalGroups: []int64{7}, FSGroup: ptr.To[int64](2000)},
			want: User{1000, 0, []uint32{7, 2000}, "/"}},
		{image: "", csc: &corev1.SecurityContext{RunAsUser: ptr.To[int64](101), RunAsGroup: ptr.To[int64](5)}, want: User{101, 5, []uint32{33}, "/var/cache/nginx"}},
		{image: "", sc: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true)}, err: "runAsNonRoot"},
		{image: "ghost", err: "unable to find user ghost"},
	}
	for _, tc := range cases {
		pod.Spec.SecurityContext = tc.sc
		c.SecurityContext = tc.csc
		got, err := ResolveUser(pod, c, &hull.ImageConfig{User: tc.image}, rootfs)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("user %q: err %v, want %q", tc.image, err, tc.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("user %q sc %+v: %+v, %v; want %+v", tc.image, tc.sc, got, err, tc.want)
		}
	}
}

func TestHostsFile(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "ns"}, Spec: corev1.PodSpec{
		Subdomain:   "web",
		HostAliases: []corev1.HostAlias{{IP: "1.2.3.4", Hostnames: []string{"a", "b"}}},
	}}
	got := HostsFile(pod, "10.87.0.12", "cluster.local")
	for _, want := range []string{"127.0.0.1\tlocalhost\n", "10.87.0.12\tweb-0.web.ns.svc.cluster.local\tweb-0\n", "# Entries added by HostAliases.\n1.2.3.4\ta\tb\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("hosts file lacks %q:\n%s", want, got)
		}
	}
}

func TestPodResources(t *testing.T) {
	lim := func(cpu, mem string) corev1.ResourceRequirements {
		return corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)}}
	}
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "i", Resources: lim("3", "2Gi")}, {Name: "s", RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways), Resources: lim("500m", "128Mi")}},
		Containers:     []corev1.Container{{Name: "a"}, {Name: "b", Resources: lim("1", "256Mi")}},
	}}
	cpus, mem := PodResources(pod)
	if want := min(3, runtime.NumCPU()); cpus != want || mem != 2048+BaseMemMB {
		t.Errorf("PodResources = %d, %d; want %d, %d", cpus, mem, want, 2048+BaseMemMB)
	}
	pod.Spec.InitContainers = pod.Spec.InitContainers[1:]
	if cpus, mem := PodResources(pod); cpus != min(3, runtime.NumCPU()) || mem != 512+256+128+BaseMemMB {
		t.Errorf("PodResources without the big init = %d, %d", cpus, mem)
	}
}
