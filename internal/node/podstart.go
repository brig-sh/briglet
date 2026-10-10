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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/brig-sh/briglet/internal/guest"
	"github.com/brig-sh/briglet/internal/hull"
	"github.com/brig-sh/briglet/internal/podvm"
)

// start boots the pod's VM: address, volumes, the image of every
// container, pod.json and the guest directory, then hull run of the base
// image with briglet-guest as its command.
func (w *podWorker) start(ctx context.Context) error {
	pod, _ := w.current()
	pod = w.effectivePod(ctx, pod)
	h := w.m.hull
	st := w.snapshot()

	waiting := func(container, reason, msg string) {
		w.set(func(st *podState) { st.Waiting = &waitingState{Container: container, Reason: reason, Message: msg} })
	}
	configErr := func(container string, err error) error {
		waiting(container, "CreateContainerConfigError", err.Error())
		w.event(corev1.EventTypeWarning, "Failed", "Error: %v", err)
		return err
	}
	bin := filepath.Join(w.m.guestBinDir, "briglet-guest")
	if _, err := os.Stat(bin); err != nil {
		waiting("", "RunContainerError", fmt.Sprintf("the guest supervisor is missing: %v; run `make guest` and `briglet up`", err))
		return err
	}
	ip, err := w.m.ipam.Allocate(string(pod.UID), pod.Namespace+"/"+pod.Name)
	if err != nil {
		waiting("", "CreateContainerError", err.Error())
		return err
	}
	w.set(func(st *podState) { st.IP = ip })

	dirs := w.podDirs()
	// Volumes come first, as in the kubelet: a pod whose ConfigMap is
	// missing stays ContainerCreating with FailedMount events.
	if err := w.writeVolumes(ctx, pod); err != nil {
		waiting("", "ContainerCreating", err.Error())
		w.event(corev1.EventTypeWarning, "FailedMount", "%v", err)
		return err
	}
	w.lastMeta = metaKey(pod)

	base, err := w.ensureImage(ctx, "", w.m.baseImage, "")
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dirs.Subpath); err != nil {
		return err
	}
	gp := &guest.Pod{
		Namespace: pod.Namespace, Name: pod.Name, UID: string(pod.UID),
		Hostname:              podvm.Hostname(pod),
		RestartPolicy:         string(pod.Spec.RestartPolicy),
		ShareProcessNamespace: pod.Spec.ShareProcessNamespace != nil && *pod.Spec.ShareProcessNamespace,
		GraceSeconds:          podvm.GraceSeconds(pod),
		Sysctls:               podvm.Sysctls(pod),
	}
	if gp.RestartPolicy == "" {
		gp.RestartPolicy = guest.RestartAlways
	}
	var hs podvm.HostShares
	images := map[string]string{}
	imageShares := map[string]hull.Share{}
	for i, c := range podvm.AllContainers(pod) {
		img, err := w.ensureImage(ctx, c.Name, c.Image, c.ImagePullPolicy)
		if err != nil {
			return err
		}
		cfg, err := h.Config(img.Digest)
		if err != nil {
			return configErr(c.Name, err)
		}
		link, short, err := w.m.imageLink(img.Digest)
		if err != nil {
			return configErr(c.Name, err)
		}
		gRootfs := guest.ImagesDir + "/" + short
		imageShares[short] = hull.Share{Host: link, Guest: gRootfs, ReadOnly: true}
		images[c.Name] = imageID(c.Image, img)
		env, err := podvm.Env(ctx, podvm.EnvInput{
			Pod: pod, Container: c, Image: cfg, Facts: w.facts(pod),
			Services: w.m.serviceEnv(pod), Src: w.m.src,
		})
		if err != nil {
			return configErr(c.Name, err)
		}
		gc, err := podvm.Container(pod, podvm.ContainerInput{
			Container: c, Init: i < len(pod.Spec.InitContainers), Index: i, Image: cfg,
			Rootfs: filepath.Join(h.StoreDir, "images", img.Digest, "rootfs"), GuestRootfs: gRootfs, Env: env,
		}, dirs, &hs)
		if err != nil {
			return configErr(c.Name, err)
		}
		gc.RestartCount = st.Restarts[c.Name]
		gp.Containers = append(gp.Containers, gc)
	}
	if err := w.writeGuestDir(pod, gp, ip); err != nil {
		waiting("", "CreateContainerError", err.Error())
		return err
	}

	shares := []hull.Share{
		{Host: w.m.guestBinDir, Guest: guest.BinDir, ReadOnly: true},
		{Host: dirs.Guest, Guest: guest.PodDir},
	}
	for _, sh := range imageShares {
		shares = append(shares, sh)
	}
	shares = append(shares, hs.Shares...)
	cpus, mem := podvm.PodResources(pod)

	// No restart in hull: a previous VM under the name is removed first.
	if err := h.Rm(ctx, st.Instance, true); err != nil && !errors.Is(err, hull.ErrNotFound) {
		w.logf("rm stale %s: %v", st.Instance, err)
	}
	spec := hull.RunSpec{
		Name:        st.Instance,
		Image:       hull.MirrorRef(w.m.baseImage, w.m.registryMirror),
		Pull:        "missing",
		MemMB:       mem,
		CPUs:        cpus,
		GatewaySock: w.m.gwSock,
		GatewayCIDR: fmt.Sprintf("%s/%d", ip, w.m.ipam.Bits()),
		Shares:      shares,
		Argv:        []string{guest.Bin, "run", guest.PodFile},
	}
	var shareArgs []string
	for _, sh := range shares {
		shareArgs = append(shareArgs, sh.Guest)
	}
	w.logf("hull run %s: %d vCPU, %d MB, %s, base %s (%s), %d containers, shares %q",
		st.Instance, cpus, mem, spec.GatewayCIDR, w.m.baseImage, base.Digest[:min(19, len(base.Digest))], len(gp.Containers), shareArgs)
	t0 := time.Now()
	rctx, cancel := context.WithTimeout(ctx, runTimeout)
	_, err = h.Run(rctx, spec)
	cancel()
	if err != nil {
		waiting("", "RunContainerError", err.Error())
		w.event(corev1.EventTypeWarning, "Failed", "Error: %v", err)
		_ = h.Rm(ctx, st.Instance, true)
		return err
	}
	w.logf("VM %s up in %.1fs", st.Instance, time.Since(t0).Seconds())
	w.set(func(st *podState) {
		st.Created, st.Running, st.StartedAt = true, true, time.Now()
		st.Waiting = nil
		st.Images = images
		st.Done, st.Phase, st.Reason, st.Message = false, "", "", ""
	})
	w.ensureForwards(ctx, pod)
	go w.keepPathWarm(ctx, st.Instance)
	return nil
}

// pathWarmFor is how long after boot the gateway keeps dialing the pod.
const pathWarmFor = 90 * time.Second

// keepPathWarm dials a closed port of the pod every few seconds right after
// boot. The gateway reaches a guest that saw no traffic for its first ~20 s
// only after two dials timed out (about 6 s), and a probe that lands in
// that window fails although the container is healthy: the liveness tests
// saw one spurious restart. Steady traffic keeps the path up, and a dial
// that is refused proves the path works. It stops when the VM is replaced
// or the pod stops.
func (w *podWorker) keepPathWarm(ctx context.Context, instance string) {
	deadline := time.Now().Add(pathWarmFor)
	for time.Now().Before(deadline) && !w.stopping() {
		st := w.snapshot()
		if !st.Running || st.Instance != instance || st.IP == "" {
			return
		}
		dctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		_, _ = w.m.gw.ProbeTCP(dctx, net.JoinHostPort(st.IP, "1"))
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// writeGuestDir writes what the supervisor reads at boot: pod.json, the
// pod's hosts, hostname and resolv.conf, an empty termination message file
// per container, and the log directory. The status.json of an earlier VM
// is removed.
func (w *podWorker) writeGuestDir(pod *corev1.Pod, gp *guest.Pod, ip string) error {
	dirs := w.podDirs()
	for _, d := range []string{"etc", "term", "logs"} {
		if err := os.MkdirAll(filepath.Join(dirs.Guest, d), 0o755); err != nil {
			return err
		}
	}
	resolv := podvm.ResolvConf(pod, w.m.dns)
	if resolv == "" {
		resolv = "nameserver " + w.m.dns.Upstream + "\n"
	}
	files := map[string]string{
		filepath.Join(dirs.Guest, "etc", "hosts"):       podvm.HostsFile(pod, ip, w.m.dns.Domain),
		filepath.Join(dirs.Guest, "etc", "hostname"):    gp.Hostname + "\n",
		filepath.Join(dirs.Guest, "etc", "resolv.conf"): resolv,
	}
	for _, c := range gp.Containers {
		files[podvm.TermFile(dirs, c.Name)] = ""
	}
	for p, content := range files {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(dirs.Guest, "status.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	b, err := json.MarshalIndent(gp, "", "  ")
	if err != nil {
		return err
	}
	// The env, Secret values included, is in pod.json. The pod dir is
	// 0700; containers do not see the guest directory.
	return os.WriteFile(filepath.Join(dirs.Guest, "pod.json"), append(b, '\n'), 0o600)
}

// imageLink returns a colon-free path to the image's rootfs, which hull can
// take as a share, and the short id the guest mounts it under.
func (m *podManager) imageLink(digest string) (link, short string, err error) {
	hex := strings.TrimPrefix(digest, "sha256:")
	if len(hex) < 12 || strings.ContainsAny(hex, ":/") {
		return "", "", fmt.Errorf("unexpected image digest %q", digest)
	}
	short = hex[:12]
	link = filepath.Join(m.imageLinks, hex)
	target := filepath.Join(m.hull.StoreDir, "images", digest, "rootfs")
	if cur, err := os.Readlink(link); err == nil && cur == target {
		return link, short, nil
	}
	if err := os.MkdirAll(m.imageLinks, 0o700); err != nil {
		return "", "", err
	}
	tmp := link + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return "", "", err
	}
	return link, short, os.Rename(tmp, link)
}

// metaKey fingerprints what a downwardAPI volume can read from the pod
// metadata that changes while the pod runs. fmt prints maps sorted.
func metaKey(pod *corev1.Pod) string {
	return fmt.Sprint(pod.Labels, pod.Annotations)
}

// ensureImage applies the pull policy and returns the image record. The
// container name, empty for the base image, labels waiting reasons.
func (w *podWorker) ensureImage(ctx context.Context, container, ref string, pullPolicy corev1.PullPolicy) (*hull.Image, error) {
	h := w.m.hull
	w.m.pullMu.Lock()
	defer w.m.pullMu.Unlock()
	// Events name the reference the pod gave; pulls and the store use the
	// mirror's.
	shown := ref
	ref = hull.MirrorRef(ref, w.m.registryMirror)
	img, local := h.LocalImage(ref)
	policy := podvm.PullPolicy(&corev1.Container{ImagePullPolicy: pullPolicy})
	waiting := func(reason, msg string) {
		w.set(func(st *podState) { st.Waiting = &waitingState{Container: container, Reason: reason, Message: msg} })
	}
	switch {
	case policy == "never" && !local:
		msg := fmt.Sprintf("Container image %q is not present with pull policy of Never", shown)
		waiting("ErrImageNeverPull", msg)
		w.event(corev1.EventTypeWarning, "ErrImageNeverPull", "%s", msg)
		return nil, errors.New(msg)
	case local && policy != "always":
		if container != "" {
			w.event(corev1.EventTypeNormal, "Pulled", "Container image %q already present on machine", shown)
		}
		return img, nil
	}
	w.event(corev1.EventTypeNormal, "Pulling", "Pulling image %q", shown)
	t0 := time.Now()
	digest, err := h.Pull(ctx, ref)
	if err == nil {
		img, err = h.ImageByDigest(digest)
	}
	if err != nil {
		waiting("ErrImagePull", err.Error())
		w.event(corev1.EventTypeWarning, "Failed", "Failed to pull image %q: %v", shown, err)
		return nil, err
	}
	w.event(corev1.EventTypeNormal, "Pulled", "Successfully pulled image %q in %s", shown, time.Since(t0).Round(time.Millisecond))
	return img, nil
}

// imageID renders the image identity like the kubelet does:
// <repository>@<digest>.
func imageID(ref string, img *hull.Image) string {
	repo := ref
	if i := strings.IndexByte(repo, '@'); i >= 0 {
		repo = repo[:i]
	} else if i := strings.LastIndexByte(repo, ':'); i >= 0 && !strings.Contains(repo[i+1:], "/") {
		repo = repo[:i]
	}
	first, _, found := strings.Cut(repo, "/")
	if !found || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		if !found {
			repo = "library/" + repo
		}
		repo = "docker.io/" + repo
	}
	return repo + "@" + img.ID()
}

// maybeRefreshVolumes rewrites the volumes when a ConfigMap or Secret they
// read changed, the pod metadata changed, or a token is due.
func (w *podWorker) maybeRefreshVolumes(ctx context.Context, pod *corev1.Pod) {
	meta := metaKey(pod)
	tr := w.snapshot().TokenRefreshAt
	dirty := w.takeVolumesDirty()
	if !dirty && meta == w.lastMeta && (tr.IsZero() || time.Now().Before(tr)) {
		return
	}
	w.lastMeta = meta
	changed, err := w.refreshVolumes(ctx, pod)
	if err != nil {
		w.logf("refresh volumes: %v", err)
		w.event(corev1.EventTypeWarning, "FailedMount", "%v", err)
		return
	}
	if changed {
		w.logf("volumes updated")
	}
}

// gcVolumes removes timestamp directories the volumes retired.
func (w *podWorker) gcVolumes(pod *corev1.Pod) {
	dirs := w.podDirs()
	for i := range pod.Spec.Volumes {
		if v := &pod.Spec.Volumes[i]; podvm.Atomic(v) {
			_ = podvm.GCAtomic(dirs.VolumeDir(v.Name), time.Now())
		}
	}
}
