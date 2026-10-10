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
	"path"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/brig-sh/briglet/internal/podvm"
)

// serviceAccountVolume is the name of the token volume the agent adds when
// the ServiceAccount admission plugin did not.
const serviceAccountVolume = "kube-api-access-briglet"

func (w *podWorker) podDirs() podvm.PodDirs {
	return podvm.NewPodDirs(podDir(w.m.podsDir, string(w.uid)))
}

// facts are the downward API values of the pod as the agent runs it. A
// limit a container does not set resolves to the VM's size.
func (w *podWorker) facts(pod *corev1.Pod) podvm.PodFacts {
	cpus, mem := podvm.PodResources(pod)
	return podvm.PodFacts{PodIP: w.snapshot().IP, HostIP: w.m.hostIP, NodeName: w.m.node, CPUs: cpus, MemMB: mem}
}

// effectivePod returns the pod as the agent runs it: with the service
// account token volume when the pod should have one and does not. The
// ServiceAccount admission plugin normally adds it as kube-api-access-*;
// the agent adds the same volume itself when it is missing, as the default
// projected volume the kubelet expects.
func (w *podWorker) effectivePod(ctx context.Context, pod *corev1.Pod) *corev1.Pod {
	if !w.decidedSA {
		w.injectSA = w.wantsServiceAccountVolume(ctx, pod)
		w.decidedSA = true
	}
	if !w.injectSA {
		return pod
	}
	p := pod.DeepCopy()
	p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
		Name: serviceAccountVolume,
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: ptr.To[int32](0o644),
			Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: ptr.To[int64](3607)}},
				{ConfigMap: &corev1.ConfigMapProjection{
					LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
					Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
				}},
				{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{
					Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"},
				}}}},
			},
		}},
	})
	for _, c := range podvm.AllContainers(p) {
		if !mountsServiceAccount(c) {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
				Name: serviceAccountVolume, MountPath: podvm.ServiceAccountMountPath, ReadOnly: true,
			})
		}
	}
	return p
}

func mountsServiceAccount(c *corev1.Container) bool {
	for _, m := range c.VolumeMounts {
		if path.Clean(m.MountPath) == podvm.ServiceAccountMountPath {
			return true
		}
	}
	return false
}

func (w *podWorker) wantsServiceAccountVolume(ctx context.Context, pod *corev1.Pod) bool {
	if mountsServiceAccount(&pod.Spec.Containers[0]) {
		return false
	}
	if a := pod.Spec.AutomountServiceAccountToken; a != nil {
		return *a
	}
	name := pod.Spec.ServiceAccountName
	if name == "" {
		name = "default"
	}
	sa, err := w.m.cs.CoreV1().ServiceAccounts(pod.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		w.logf("service account %s: %v; not mounting a token", name, err)
		return false
	}
	return sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken
}

// writeVolumes creates the emptyDir and hostPath directories and writes the
// files of every ConfigMap, Secret, downwardAPI and projected volume. It
// subscribes the pod to changes of the objects those volumes read.
func (w *podWorker) writeVolumes(ctx context.Context, pod *corev1.Pod) error {
	if err := podvm.PrepareVolumes(pod, w.podDirs()); err != nil {
		return err
	}
	w.m.sources.Set(w.uid, volumeKeys(pod), w.volumesChanged)
	_, err := w.refreshVolumes(ctx, pod)
	return err
}

// refreshVolumes rewrites the atomically written volumes whose content
// changed and fetches due tokens. It reports whether a volume changed.
func (w *podWorker) refreshVolumes(ctx context.Context, pod *corev1.Pod) (bool, error) {
	facts := w.facts(pod)
	dirs := w.podDirs()
	var refreshAt time.Time
	changed := false
	for i := range pod.Spec.Volumes {
		v := &pod.Spec.Volumes[i]
		if !podvm.Atomic(v) {
			continue
		}
		payload, r, err := podvm.Payload(ctx, pod, v, facts, w.m.src)
		if err != nil {
			return changed, fmt.Errorf("MountVolume.SetUp failed for volume %q : %w", v.Name, err)
		}
		did, err := podvm.WriteAtomic(dirs.VolumeDir(v.Name), payload, time.Now())
		if err != nil {
			return changed, fmt.Errorf("MountVolume.SetUp failed for volume %q : %w", v.Name, err)
		}
		changed = changed || did
		if !r.IsZero() && (refreshAt.IsZero() || r.Before(refreshAt)) {
			refreshAt = r
		}
	}
	w.set(func(st *podState) { st.TokenRefreshAt = refreshAt })
	return changed, nil
}

// volumesChanged runs on an informer goroutine when a ConfigMap or Secret a
// volume reads changed. The worker rewrites the files on its next turn.
func (w *podWorker) volumesChanged() {
	w.mu.Lock()
	w.volumesDirty = true
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *podWorker) takeVolumesDirty() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	d := w.volumesDirty
	w.volumesDirty = false
	return d
}
