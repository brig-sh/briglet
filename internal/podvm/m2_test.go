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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	"github.com/brig-sh/briglet/internal/hull"
)

type fakeSources struct {
	cms    map[string]*corev1.ConfigMap
	secs   map[string]*corev1.Secret
	tokens int
}

func (f *fakeSources) ConfigMap(_ context.Context, _, name string) (*corev1.ConfigMap, error) {
	if cm, ok := f.cms[name]; ok {
		return cm, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
}

func (f *fakeSources) Secret(_ context.Context, _, name string) (*corev1.Secret, error) {
	if s, ok := f.secs[name]; ok {
		return s, nil
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
}

func (f *fakeSources) Token(_ context.Context, _ *corev1.Pod, aud string, exp int64) (Token, error) {
	f.tokens++
	return Token{Value: "tok-" + aud, RefreshAt: time.Unix(1000+exp, 0)}, nil
}

func testSources() *fakeSources {
	return &fakeSources{
		cms: map[string]*corev1.ConfigMap{"cfg": {
			Data:       map[string]string{"a": "1", "index.html": "<h1>hi</h1>"},
			BinaryData: map[string][]byte{"bin": {0, 1}},
		}},
		secs: map[string]*corev1.Secret{"sec": {Data: map[string][]byte{"password": []byte("s3cret"), "user": []byte("bob")}}},
	}
}

func TestEnv(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns", UID: "u-1", Labels: map[string]string{"app": "web"}},
		Spec:       corev1.PodSpec{ServiceAccountName: "sa"},
	}
	c := &corev1.Container{
		EnvFrom: []corev1.EnvFromSource{
			{Prefix: "CM_", ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"}}},
			{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "sec"}}},
			{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "nope"}, Optional: ptr.To(true)}},
		},
		Env: []corev1.EnvVar{
			{Name: "PATH", Value: "/x:$(PATH)"},
			{Name: "IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
			{Name: "APP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels['app']"}}},
			{Name: "SA", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.serviceAccountName"}}},
			{Name: "A", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"}, Key: "a"}}},
			{Name: "PW", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "sec"}, Key: "password"}}},
			{Name: "OPT", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "sec"}, Key: "missing", Optional: ptr.To(true)}}},
			{Name: "MEM", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{
				Resource: "limits.memory", Divisor: resource.MustParse("1Mi")}}},
			{Name: "CPU", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}}},
			{Name: "REQ", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "requests.cpu", Divisor: resource.MustParse("1m")}}},
		},
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}},
	}
	got, err := Env(context.Background(), EnvInput{
		Pod: pod, Container: c, Image: &hull.ImageConfig{Env: []string{"PATH=/bin"}},
		Facts:    PodFacts{PodIP: "10.87.0.10", CPUs: 1, MemMB: 512},
		Services: []EnvVar{{Name: "KUBERNETES_SERVICE_HOST", Value: "10.96.0.1"}},
		Src:      testSources(),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []EnvVar{
		{Name: "HOSTNAME", Value: "web"},
		{Name: "PATH", Value: "/x:/bin"},
		{Name: "KUBERNETES_SERVICE_HOST", Value: "10.96.0.1"},
		{Name: "CM_a", Value: "1"},
		{Name: "CM_index.html", Value: "<h1>hi</h1>"},
		{Name: "password", Value: "s3cret", Secret: true},
		{Name: "user", Value: "bob", Secret: true},
		{Name: "IP", Value: "10.87.0.10"},
		{Name: "APP", Value: "web"},
		{Name: "SA", Value: "sa"},
		{Name: "A", Value: "1"},
		{Name: "PW", Value: "s3cret", Secret: true},
		{Name: "MEM", Value: "512"},
		{Name: "CPU", Value: "1"},
		{Name: "REQ", Value: "250"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Env =\n%v\nwant\n%v", got, want)
	}

	c.Env = []corev1.EnvVar{{Name: "S", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "absent"}, Key: "k"}}}}
	c.EnvFrom = nil
	_, err = Env(context.Background(), EnvInput{Pod: pod, Container: c, Image: &hull.ImageConfig{}, Src: testSources()})
	if !apierrors.IsNotFound(err) {
		t.Errorf("missing secret: err = %v, want NotFound", err)
	}

	plain, secret, err := HullEnv(want)
	if err != nil || len(secret) != 3 || strings.Contains(strings.Join(plain, " "), "s3cret") {
		t.Errorf("HullEnv: plain %q secret %q err %v", plain, secret, err)
	}
	if _, _, err := HullEnv([]EnvVar{{Name: "HOME", Value: "x", Secret: true}}); err == nil {
		t.Error("HullEnv: a secret HOME passed")
	}
	if _, _, err := HullEnv([]EnvVar{{Name: "CERT", Value: "a\nb"}}); err == nil {
		t.Error("HullEnv: a value with a newline passed")
	}
}

func TestServiceEnv(t *testing.T) {
	svc := func(ns, name, ip string, ports ...corev1.ServicePort) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: corev1.ServiceSpec{ClusterIP: ip, Ports: ports}}
	}
	svcs := []*corev1.Service{
		svc("default", "kubernetes", "10.96.0.1", corev1.ServicePort{Name: "https", Port: 443, Protocol: corev1.ProtocolTCP}),
		svc("app", "my-web", "10.96.0.20", corev1.ServicePort{Name: "http", Port: 80}),
		svc("app", "headless", "None", corev1.ServicePort{Port: 80}),
		svc("other", "db", "10.96.0.30", corev1.ServicePort{Port: 5432}),
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "app"}}
	got := map[string]string{}
	for _, v := range ServiceEnv(pod, svcs) {
		got[v.Name] = v.Value
	}
	for k, v := range map[string]string{
		"KUBERNETES_SERVICE_HOST":       "10.96.0.1",
		"KUBERNETES_SERVICE_PORT":       "443",
		"KUBERNETES_SERVICE_PORT_HTTPS": "443",
		"KUBERNETES_PORT":               "tcp://10.96.0.1:443",
		"KUBERNETES_PORT_443_TCP":       "tcp://10.96.0.1:443",
		"KUBERNETES_PORT_443_TCP_PROTO": "tcp",
		"KUBERNETES_PORT_443_TCP_PORT":  "443",
		"KUBERNETES_PORT_443_TCP_ADDR":  "10.96.0.1",
		"MY_WEB_SERVICE_HOST":           "10.96.0.20",
		"MY_WEB_SERVICE_PORT_HTTP":      "80",
		"MY_WEB_PORT_80_TCP_ADDR":       "10.96.0.20",
	} {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["DB_SERVICE_HOST"]; ok {
		t.Error("a Service of another namespace leaked in")
	}
	if len(got) != 8+8 {
		t.Errorf("got %d variables: %v", len(got), got)
	}
	pod.Spec.EnableServiceLinks = ptr.To(false)
	if n := len(ServiceEnv(pod, svcs)); n != 8 {
		t.Errorf("enableServiceLinks false: %d variables, want only the 8 of kubernetes", n)
	}
}

func readLink(t *testing.T, p string) string {
	t.Helper()
	s, err := os.Readlink(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWriteAtomic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vol")
	t0 := time.Now().Add(-10 * time.Minute)
	p1 := map[string]FileProjection{"a": {Data: []byte("1"), Mode: 0o644}, "sub/b": {Data: []byte("2"), Mode: 0o400}}
	changed, err := WriteAtomic(dir, p1, t0)
	if err != nil || !changed {
		t.Fatalf("first write: %v %v", changed, err)
	}
	first := readLink(t, filepath.Join(dir, "..data"))
	if readLink(t, filepath.Join(dir, "a")) != "..data/a" || readLink(t, filepath.Join(dir, "sub")) != "..data/sub" {
		t.Error("top-level symlinks missing")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "sub", "b")); string(b) != "2" {
		t.Errorf("sub/b = %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "sub", "b")); fi.Mode().Perm() != 0o400 {
		t.Errorf("sub/b mode %v", fi.Mode())
	}
	if changed, err := WriteAtomic(dir, p1, t0); err != nil || changed {
		t.Errorf("same payload rewritten: %v %v", changed, err)
	}
	p2 := map[string]FileProjection{"a": {Data: []byte("v2"), Mode: 0o644}}
	if changed, err := WriteAtomic(dir, p2, t0.Add(time.Second)); err != nil || !changed {
		t.Fatalf("second write: %v %v", changed, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a")); string(b) != "v2" {
		t.Errorf("a = %q after update", b)
	}
	if _, err := os.Lstat(filepath.Join(dir, "sub")); !os.IsNotExist(err) {
		t.Error("stale top-level symlink kept")
	}
	if _, err := os.Stat(filepath.Join(dir, first)); err != nil {
		t.Errorf("retired dir removed at once: %v", err)
	}
	if err := GCAtomic(dir, t0.Add(time.Second+RetainRetired+time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, first)); !os.IsNotExist(err) {
		t.Error("retired dir kept after RetainRetired")
	}
	if _, err := WriteAtomic(dir, map[string]FileProjection{"../x": {}}, t0); err == nil {
		t.Error("path escaping the volume accepted")
	}
}

func TestPayload(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns", Labels: map[string]string{"b": "2", "a": "1"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}},
	}
	src := testSources()
	ctx := context.Background()
	f := PodFacts{CPUs: 2, MemMB: 256}

	vol := corev1.Volume{Name: "v", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"}}}}
	got, _, err := Payload(ctx, pod, &vol, f, src)
	if err != nil || len(got) != 3 || string(got["bin"].Data) != "\x00\x01" || got["a"].Mode != 0o644 {
		t.Errorf("configMap payload %v %v", got, err)
	}

	vol.VolumeSource = corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "sec", DefaultMode: ptr.To[int32](0o400),
		Items: []corev1.KeyToPath{{Key: "password", Path: "dir/pw"}, {Key: "user", Path: "u", Mode: ptr.To[int32](0o440)}}}}
	got, _, err = Payload(ctx, pod, &vol, f, src)
	if err != nil || string(got["dir/pw"].Data) != "s3cret" || got["dir/pw"].Mode != 0o400 || got["u"].Mode != 0o440 {
		t.Errorf("secret payload %v %v", got, err)
	}

	vol.VolumeSource = corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "absent"}}}
	if _, _, err := Payload(ctx, pod, &vol, f, src); !apierrors.IsNotFound(err) {
		t.Errorf("missing configmap: %v", err)
	}
	vol.ConfigMap.Optional = ptr.To(true)
	if got, _, err := Payload(ctx, pod, &vol, f, src); err != nil || len(got) != 0 {
		t.Errorf("missing optional configmap: %v %v", got, err)
	}

	vol.VolumeSource = corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{Items: []corev1.DownwardAPIVolumeFile{
		{Path: "name", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
		{Path: "labels", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels"}},
		{Path: "mem", ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: "c", Resource: "limits.memory", Divisor: resource.MustParse("1Mi")}},
	}}}
	got, _, err = Payload(ctx, pod, &vol, f, src)
	if err != nil || string(got["name"].Data) != "web" || string(got["labels"].Data) != "a=\"1\"\nb=\"2\"" || string(got["mem"].Data) != "256" {
		t.Errorf("downwardAPI payload %q %v", got, err)
	}

	vol.VolumeSource = corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
		{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", Audience: "vault", ExpirationSeconds: ptr.To[int64](600)}},
		{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"}, Items: []corev1.KeyToPath{{Key: "a", Path: "ca.crt"}}}},
		{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}}},
	}}}
	got, refresh, err := Payload(ctx, pod, &vol, f, src)
	if err != nil || string(got["token"].Data) != "tok-vault" || string(got["ca.crt"].Data) != "1" || string(got["namespace"].Data) != "ns" || refresh.Unix() != 1600 {
		t.Errorf("projected payload %q refresh %v %v", got, refresh, err)
	}
}

func TestShares(t *testing.T) {
	root := t.TempDir()
	dirs := PodDirs{Volumes: filepath.Join(root, "volumes"), Subpath: filepath.Join(root, "subpath")}
	host := filepath.Join(root, "host")
	if err := os.MkdirAll(filepath.Join(host, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "file.conf"), []byte("conf"), 0o644); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{
		{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"}}}},
		{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: host}}},
	}}}
	c := &corev1.Container{VolumeMounts: []corev1.VolumeMount{
		{Name: "cache", MountPath: "/cache"},
		{Name: "cache", MountPath: "/logs", SubPathExpr: "$(POD)/logs"},
		{Name: "cfg", MountPath: "/usr/share/nginx/html"},
		{Name: "cfg", MountPath: "/etc/app/index.html", SubPath: "index.html"},
		{Name: "host", MountPath: "/etc/app/app.conf", SubPath: "file.conf", ReadOnly: true},
		{Name: "host", MountPath: "/data", SubPath: "sub", ReadOnly: true},
	}}
	if err := PrepareVolumes(pod, dirs); err != nil {
		t.Fatal(err)
	}
	payload, _, err := Payload(context.Background(), pod, &pod.Spec.Volumes[1], PodFacts{}, testSources())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteAtomic(dirs.VolumeDir("cfg"), payload, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := Shares(pod, c, dirs, []EnvVar{{Name: "POD", Value: "web-1"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []hull.Share{
		{Host: dirs.VolumeDir("cache"), Guest: "/cache"},
		{Host: filepath.Join(dirs.VolumeDir("cache"), "web-1", "logs"), Guest: "/logs"},
		{Host: dirs.VolumeDir("cfg"), Guest: "/usr/share/nginx/html", ReadOnly: true},
		{Host: filepath.Join(host, "sub"), Guest: "/data", ReadOnly: true},
		{Host: filepath.Join(dirs.Subpath, "f0"), Guest: "/etc/app", ReadOnly: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Shares =\n%+v\nwant\n%+v", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dirs.Subpath, "f0", "index.html")); string(b) != "<h1>hi</h1>" {
		t.Errorf("file subPath copy = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dirs.Subpath, "f0", "app.conf")); string(b) != "conf" {
		t.Errorf("hostPath file subPath link = %q", b)
	}
	c.VolumeMounts = []corev1.VolumeMount{{Name: "host", MountPath: "/etc/x.conf", SubPath: "file.conf"}}
	if _, err := Shares(pod, c, dirs, nil); err == nil {
		t.Error("a file subPath covering /etc was accepted")
	}
	c.VolumeMounts = []corev1.VolumeMount{{Name: "cache", MountPath: "/x", SubPathExpr: "$(UP)"}}
	if _, err := Shares(pod, c, dirs, []EnvVar{{Name: "UP", Value: "../.."}}); err == nil {
		t.Error("a subPathExpr leaving the volume was accepted")
	}
}

func TestValidateVolumes(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c"}}, Volumes: []corev1.Volume{
		{Name: "pvc", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "x"}}},
	}}}
	if r := Validate(pod); !strings.Contains(r, "persistentVolumeClaim") {
		t.Errorf("Validate(pvc) = %q", r)
	}
	pod.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	if r := Validate(pod); r != "" {
		t.Errorf("Validate(emptyDir) = %q", r)
	}
}
