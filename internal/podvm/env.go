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
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/brig-sh/briglet/internal/hull"
)

// Sources reads the objects a pod refers to. A missing object is reported
// with an error for which apierrors.IsNotFound is true.
type Sources interface {
	ConfigMap(ctx context.Context, ns, name string) (*corev1.ConfigMap, error)
	Secret(ctx context.Context, ns, name string) (*corev1.Secret, error)
	// Token returns a service account token for the pod with the audience
	// and lifetime asked for, and when it should be refreshed.
	Token(ctx context.Context, pod *corev1.Pod, audience string, expirationSeconds int64) (Token, error)
}

// PodFacts are the runtime values the downward API exposes.
type PodFacts struct {
	PodIP    string
	HostIP   string
	NodeName string
	// CPUs and MemMB are the size of the VM. They stand in for limits the
	// container does not set.
	CPUs  int
	MemMB int
}

// EnvVar is one variable of the container environment. Secret marks a value
// that came from a Secret and must stay off command lines.
type EnvVar struct {
	Name   string
	Value  string
	Secret bool
}

// EnvInput is what the container environment is computed from.
type EnvInput struct {
	Pod       *corev1.Pod
	Container *corev1.Container
	Image     *hull.ImageConfig
	Facts     PodFacts
	// Services are the Service links, from ServiceEnv.
	Services []EnvVar
	Src      Sources
}

// Env computes the container environment: HOSTNAME, the image ENV, the
// Service links, envFrom, then env. Later entries win, as in the kubelet,
// where the container runtime puts the container env over the image ENV.
func Env(ctx context.Context, in EnvInput) ([]EnvVar, error) {
	pod, c := in.Pod, in.Container
	var order []string
	vals := map[string]EnvVar{}
	set := func(v EnvVar) {
		if _, ok := vals[v.Name]; !ok {
			order = append(order, v.Name)
		}
		vals[v.Name] = v
	}
	plain := func() map[string]string {
		m := make(map[string]string, len(vals))
		for k, v := range vals {
			m[k] = v.Value
		}
		return m
	}
	set(EnvVar{Name: "HOSTNAME", Value: pod.Name})
	for _, kv := range in.Image.Env {
		k, v, _ := strings.Cut(kv, "=")
		set(EnvVar{Name: k, Value: v})
	}
	for _, v := range in.Services {
		set(v)
	}
	for _, from := range c.EnvFrom {
		switch {
		case from.ConfigMapRef != nil:
			ref := from.ConfigMapRef
			cm, err := in.Src.ConfigMap(ctx, pod.Namespace, ref.Name)
			if apierrors.IsNotFound(err) && optional(ref.Optional) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("envFrom configmap %q: %w", ref.Name, err)
			}
			for _, k := range sortedKeys(cm.Data) {
				if validEnvName(from.Prefix + k) {
					set(EnvVar{Name: from.Prefix + k, Value: cm.Data[k]})
				}
			}
		case from.SecretRef != nil:
			ref := from.SecretRef
			sec, err := in.Src.Secret(ctx, pod.Namespace, ref.Name)
			if apierrors.IsNotFound(err) && optional(ref.Optional) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("envFrom secret %q: %w", ref.Name, err)
			}
			for _, k := range sortedKeys(sec.Data) {
				if validEnvName(from.Prefix + k) {
					set(EnvVar{Name: from.Prefix + k, Value: string(sec.Data[k]), Secret: true})
				}
			}
		}
	}
	for _, e := range c.Env {
		if e.ValueFrom == nil {
			set(EnvVar{Name: e.Name, Value: expand(e.Value, plain())})
			continue
		}
		v, ok, err := valueFrom(ctx, in, e)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", e.Name, err)
		}
		if ok {
			set(v)
		}
	}
	out := make([]EnvVar, 0, len(order))
	for _, k := range order {
		out = append(out, vals[k])
	}
	return out, nil
}

func valueFrom(ctx context.Context, in EnvInput, e corev1.EnvVar) (EnvVar, bool, error) {
	pod, src := in.Pod, e.ValueFrom
	switch {
	case src.FieldRef != nil:
		v, err := FieldValue(pod, in.Facts, src.FieldRef.FieldPath)
		return EnvVar{Name: e.Name, Value: v}, err == nil, err
	case src.ResourceFieldRef != nil:
		v, err := ResourceValue(src.ResourceFieldRef, in.Container, in.Facts)
		return EnvVar{Name: e.Name, Value: v}, err == nil, err
	case src.ConfigMapKeyRef != nil:
		ref := src.ConfigMapKeyRef
		cm, err := in.Src.ConfigMap(ctx, pod.Namespace, ref.Name)
		if apierrors.IsNotFound(err) && optional(ref.Optional) {
			return EnvVar{}, false, nil
		}
		if err != nil {
			return EnvVar{}, false, fmt.Errorf("configmap %q: %w", ref.Name, err)
		}
		v, ok := cm.Data[ref.Key]
		if !ok {
			if optional(ref.Optional) {
				return EnvVar{}, false, nil
			}
			return EnvVar{}, false, fmt.Errorf("couldn't find key %s in ConfigMap %s/%s", ref.Key, pod.Namespace, ref.Name)
		}
		return EnvVar{Name: e.Name, Value: v}, true, nil
	case src.SecretKeyRef != nil:
		ref := src.SecretKeyRef
		sec, err := in.Src.Secret(ctx, pod.Namespace, ref.Name)
		if apierrors.IsNotFound(err) && optional(ref.Optional) {
			return EnvVar{}, false, nil
		}
		if err != nil {
			return EnvVar{}, false, fmt.Errorf("secret %q: %w", ref.Name, err)
		}
		v, ok := sec.Data[ref.Key]
		if !ok {
			if optional(ref.Optional) {
				return EnvVar{}, false, nil
			}
			return EnvVar{}, false, fmt.Errorf("couldn't find key %s in Secret %s/%s", ref.Key, pod.Namespace, ref.Name)
		}
		return EnvVar{Name: e.Name, Value: string(v), Secret: true}, true, nil
	}
	return EnvVar{}, false, fmt.Errorf("valueFrom names no source briglet supports")
}

func optional(b *bool) bool { return b != nil && *b }

// validEnvName is the relaxed rule of current Kubernetes: printable ASCII
// without '='. The kubelet skips envFrom keys that break it.
func validEnvName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < ' ' || r > '~' || r == '=' {
			return false
		}
	}
	return true
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Pairs renders the environment as KEY=VALUE strings.
func Pairs(env []EnvVar) []string {
	out := make([]string, 0, len(env))
	for _, v := range env {
		out = append(out, v.Name+"="+v.Value)
	}
	return out
}

// hullReads lists the names hull reads from its own environment. A value
// for one of them cannot travel in hull's environment without changing what
// hull does.
func hullReads(name string) bool {
	switch name {
	case "HOME", "PATH", "TMPDIR":
		return true
	}
	return strings.HasPrefix(name, "XDG_") || strings.HasPrefix(name, "HULL_")
}

// HullEnv splits the environment into the values that may go on the hull
// command line as KEY=VALUE and the secret ones, which go in hull's own
// process environment and are named on the command line by key only. hull
// writes the guest environment one variable per line, so a value with a
// newline is refused.
func HullEnv(env []EnvVar) (plain, secret []string, err error) {
	for _, v := range env {
		if strings.ContainsAny(v.Value, "\n\r") {
			return nil, nil, fmt.Errorf("env %s: hull passes the environment one variable per line, so a value with a newline cannot be passed", v.Name)
		}
		if !v.Secret {
			plain = append(plain, v.Name+"="+v.Value)
			continue
		}
		if hullReads(v.Name) {
			return nil, nil, fmt.Errorf("env %s: a Secret value cannot be passed under a name hull reads for itself", v.Name)
		}
		secret = append(secret, v.Name+"="+v.Value)
	}
	return plain, secret, nil
}
