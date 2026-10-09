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
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Token is a service account token and when to fetch a new one.
type Token struct {
	Value     string
	RefreshAt time.Time
}

const (
	defaultVolumeMode = 0o644
	// defaultTokenSeconds is the API default of a projected token's
	// expirationSeconds.
	defaultTokenSeconds = 3600
	// ServiceAccountMountPath is where the kubelet mounts the service
	// account token.
	ServiceAccountMountPath = "/var/run/secrets/kubernetes.io/serviceaccount"
)

// Atomic reports whether the volume's files are written by the agent with
// the atomic writer layout. Such volumes are shared read-only.
func Atomic(v *corev1.Volume) bool {
	return v.ConfigMap != nil || v.Secret != nil || v.DownwardAPI != nil || v.Projected != nil
}

// Payload builds the files of an Atomic volume. refreshAt is when a token in
// it should be fetched again, zero when it holds none.
func Payload(ctx context.Context, pod *corev1.Pod, v *corev1.Volume, f PodFacts, src Sources) (map[string]FileProjection, time.Time, error) {
	out := map[string]FileProjection{}
	var refreshAt time.Time
	switch {
	case v.ConfigMap != nil:
		s := v.ConfigMap
		err := configMapFiles(ctx, out, pod.Namespace, s.Name, s.Items, mode(s.DefaultMode), optional(s.Optional), src)
		return out, refreshAt, err
	case v.Secret != nil:
		s := v.Secret
		err := secretFiles(ctx, out, pod.Namespace, s.SecretName, s.Items, mode(s.DefaultMode), optional(s.Optional), src)
		return out, refreshAt, err
	case v.DownwardAPI != nil:
		err := downwardFiles(out, pod, v.DownwardAPI.Items, mode(v.DownwardAPI.DefaultMode), f)
		return out, refreshAt, err
	case v.Projected != nil:
		def := mode(v.Projected.DefaultMode)
		for _, ps := range v.Projected.Sources {
			var err error
			switch {
			case ps.ConfigMap != nil:
				s := ps.ConfigMap
				err = configMapFiles(ctx, out, pod.Namespace, s.Name, s.Items, def, optional(s.Optional), src)
			case ps.Secret != nil:
				s := ps.Secret
				err = secretFiles(ctx, out, pod.Namespace, s.Name, s.Items, def, optional(s.Optional), src)
			case ps.DownwardAPI != nil:
				err = downwardFiles(out, pod, ps.DownwardAPI.Items, def, f)
			case ps.ServiceAccountToken != nil:
				s := ps.ServiceAccountToken
				exp := int64(defaultTokenSeconds)
				if s.ExpirationSeconds != nil {
					exp = *s.ExpirationSeconds
				}
				var tok Token
				tok, err = src.Token(ctx, pod, s.Audience, exp)
				if err == nil {
					out[s.Path] = FileProjection{Data: []byte(tok.Value), Mode: def}
					if refreshAt.IsZero() || tok.RefreshAt.Before(refreshAt) {
						refreshAt = tok.RefreshAt
					}
				}
			default:
				err = fmt.Errorf("projected volume %s: unsupported source", v.Name)
			}
			if err != nil {
				return nil, time.Time{}, err
			}
		}
		return out, refreshAt, nil
	}
	return nil, time.Time{}, fmt.Errorf("volume %s is not written by the agent", v.Name)
}

func mode(m *int32) int32 {
	if m == nil {
		return defaultVolumeMode
	}
	return *m
}

func itemMode(m *int32, def int32) int32 {
	if m == nil {
		return def
	}
	return *m
}

func configMapFiles(ctx context.Context, out map[string]FileProjection, ns, name string, items []corev1.KeyToPath, def int32, opt bool, src Sources) error {
	cm, err := src.ConfigMap(ctx, ns, name)
	if apierrors.IsNotFound(err) && opt {
		return nil
	}
	if err != nil {
		return fmt.Errorf("configmap %q: %w", name, err)
	}
	data := map[string][]byte{}
	for k, v := range cm.Data {
		data[k] = []byte(v)
	}
	for k, v := range cm.BinaryData {
		data[k] = v
	}
	return keyFiles(out, "configmap", ns, name, data, items, def, opt)
}

func secretFiles(ctx context.Context, out map[string]FileProjection, ns, name string, items []corev1.KeyToPath, def int32, opt bool, src Sources) error {
	sec, err := src.Secret(ctx, ns, name)
	if apierrors.IsNotFound(err) && opt {
		return nil
	}
	if err != nil {
		return fmt.Errorf("secret %q: %w", name, err)
	}
	return keyFiles(out, "secret", ns, name, sec.Data, items, def, opt)
}

// keyFiles projects keys to files: every key under its own name without
// items, or the listed keys at their paths.
func keyFiles(out map[string]FileProjection, kind, ns, name string, data map[string][]byte, items []corev1.KeyToPath, def int32, opt bool) error {
	if len(items) == 0 {
		for k, v := range data {
			out[k] = FileProjection{Data: v, Mode: def}
		}
		return nil
	}
	for _, it := range items {
		v, ok := data[it.Key]
		if !ok {
			if opt {
				continue
			}
			return fmt.Errorf("%s %s/%s has no key %q", kind, ns, name, it.Key)
		}
		out[it.Path] = FileProjection{Data: v, Mode: itemMode(it.Mode, def)}
	}
	return nil
}

func downwardFiles(out map[string]FileProjection, pod *corev1.Pod, items []corev1.DownwardAPIVolumeFile, def int32, f PodFacts) error {
	for _, it := range items {
		var v string
		var err error
		switch {
		case it.FieldRef != nil:
			v, err = FieldValue(pod, f, it.FieldRef.FieldPath)
		case it.ResourceFieldRef != nil:
			c := containerNamed(pod, it.ResourceFieldRef.ContainerName)
			if c == nil {
				return fmt.Errorf("downwardAPI %s: no container %q", it.Path, it.ResourceFieldRef.ContainerName)
			}
			v, err = ResourceValue(it.ResourceFieldRef, c, f)
		}
		if err != nil {
			return fmt.Errorf("downwardAPI %s: %w", it.Path, err)
		}
		out[it.Path] = FileProjection{Data: []byte(v), Mode: itemMode(it.Mode, def)}
	}
	return nil
}

func containerNamed(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.Containers {
		if name == "" || pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

// References lists the ConfigMaps and Secrets the pod's volumes read, which
// the agent watches to refresh the files.
func References(pod *corev1.Pod) (configMaps, secrets []string) {
	cms, secs := map[string]bool{}, map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.ConfigMap != nil:
			cms[v.ConfigMap.Name] = true
		case v.Secret != nil:
			secs[v.Secret.SecretName] = true
		case v.Projected != nil:
			for _, s := range v.Projected.Sources {
				if s.ConfigMap != nil {
					cms[s.ConfigMap.Name] = true
				}
				if s.Secret != nil {
					secs[s.Secret.Name] = true
				}
			}
		}
	}
	return sortedKeys(cms), sortedKeys(secs)
}

// volumeType names the source of a volume, for messages.
func volumeType(v *corev1.Volume) string {
	b, _ := json.Marshal(v.VolumeSource)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// unsupportedVolumes lists the pod's volumes briglet cannot provide.
func unsupportedVolumes(pod *corev1.Pod) []string {
	var why []string
	for i := range pod.Spec.Volumes {
		v := &pod.Spec.Volumes[i]
		switch {
		case v.EmptyDir != nil:
			if v.EmptyDir.Medium == corev1.StorageMediumHugePages || strings.HasPrefix(string(v.EmptyDir.Medium), "HugePages") {
				why = append(why, fmt.Sprintf("emptyDir %s with medium %s", v.Name, v.EmptyDir.Medium))
			}
		case v.HostPath != nil:
			switch t := v.HostPath.Type; {
			case t == nil, *t == corev1.HostPathUnset, *t == corev1.HostPathDirectoryOrCreate, *t == corev1.HostPathDirectory,
				*t == corev1.HostPathFileOrCreate, *t == corev1.HostPathFile:
			default:
				why = append(why, fmt.Sprintf("hostPath %s of type %s", v.Name, *t))
			}
		case v.ConfigMap != nil, v.Secret != nil, v.DownwardAPI != nil:
		case v.Projected != nil:
			for _, s := range v.Projected.Sources {
				if s.ConfigMap == nil && s.Secret == nil && s.DownwardAPI == nil && s.ServiceAccountToken == nil {
					why = append(why, fmt.Sprintf("projected volume %s with a source other than configMap, secret, downwardAPI or serviceAccountToken", v.Name))
					break
				}
			}
		default:
			why = append(why, fmt.Sprintf("volume %s of type %s", v.Name, volumeType(v)))
		}
	}
	for _, c := range pod.Spec.Containers {
		if len(c.VolumeDevices) > 0 {
			why = append(why, "volumeDevices")
		}
		for _, m := range c.VolumeMounts {
			if strings.ContainsAny(m.MountPath, " \t'\"") {
				why = append(why, fmt.Sprintf("mountPath %q with spaces or quotes", m.MountPath))
			}
		}
	}
	return why
}
