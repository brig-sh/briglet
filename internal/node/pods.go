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
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	"github.com/brig-sh/briglet/internal/hull"
	"github.com/brig-sh/briglet/internal/podvm"
)

const (
	pollInterval   = 5 * time.Second
	backoffInitial = 10 * time.Second
	backoffMax     = 5 * time.Minute
	// A container that ran this long without exiting resets the back-off,
	// as the kubelet does.
	backoffReset = 10 * time.Minute
	runTimeout   = 5 * time.Minute
	// hostnameWait bounds the retries of the post-boot hostname exec. The
	// agent socket accepts before the guest agent listens.
	hostnameWait = 20 * time.Second
	// exitCodeClean and exitCodeKilled are what briglet reports for a VM
	// that ended. hull does not observe the guest's exit status: a VM whose
	// guest shut itself down reports 0, one that died otherwise reports 137.
	exitCodeClean  = 0
	exitCodeKilled = 137
)

// podManager runs one worker per pod bound to this node.
type podManager struct {
	cs       kubernetes.Interface
	node     string
	hostIP   string
	hull     *hull.Client
	ipam     *podvm.IPAM
	podsDir  string
	gwSock   string
	maxName  int
	recorder record.EventRecorder
	lister   corelisters.PodLister
	// shared is the cluster-wide informer factory the agent shares with
	// the service table sync.
	shared informers.SharedInformerFactory
	// services feeds the Service link variables of new containers.
	services corelisters.ServiceLister
	// apiServiceIP is the cluster IP of the `kubernetes` Service, used
	// for KUBERNETES_SERVICE_HOST until the Service shows up in the
	// informer.
	apiServiceIP string
	// dns is what pod resolv.conf files are built from.
	dns         podvm.DNS
	gw          *hull.Gateway
	sources     *sourceWatch
	src         *apiSources
	probeClient *http.Client
	// pullMu serializes image pulls. hull sweeps every staging directory
	// at the start of a pull, so two concurrent pulls break each other.
	pullMu sync.Mutex

	mu      sync.Mutex
	workers map[types.UID]*podWorker
	ctx     context.Context
}

func newPodManager(cs kubernetes.Interface, cfg Config, h *hull.Client, shared informers.SharedInformerFactory) (*podManager, error) {
	ipam, err := podvm.NewIPAM(cfg.IPAMFile, cfg.PodCIDR)
	if err != nil {
		return nil, err
	}
	b := record.NewBroadcaster()
	b.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: cs.CoreV1().Events("")})
	// sockaddr_un holds 104 bytes with the NUL; hull puts agent.sock in the
	// instance directory.
	maxName := 103 - len(filepath.Join(h.StoreDir, "instances")) - 1 - len("/agent.sock")
	sw := newSourceWatch(cs)
	apiIP := cfg.APIServiceIP
	if apiIP == "" {
		apiIP = "10.96.0.1"
	}
	return &podManager{
		apiServiceIP: apiIP,
		shared:       shared,
		dns:          podvm.DNS{ClusterDNS: cfg.ClusterDNS, Domain: cfg.ClusterDomain, Upstream: cfg.HostIP},
		gw:           hull.NewGateway(cfg.GatewaySock + ".api"),
		sources:      sw,
		src:          &apiSources{cs: cs, watch: sw},
		probeClient:  newProbeClient(),
		cs:           cs,
		node:         cfg.Name,
		hostIP:       cfg.HostIP,
		hull:         h,
		ipam:         ipam,
		podsDir:      cfg.PodsDir,
		gwSock:       cfg.GatewaySock,
		maxName:      maxName,
		recorder:     b.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "kubelet", Host: cfg.Name}),
		workers:      map[types.UID]*podWorker{},
	}, nil
}

// run starts the pod informer, reconciles the store against the bound pods
// once, then dispatches pod events to workers until ctx ends.
func (m *podManager) run(ctx context.Context) error {
	m.ctx = ctx
	factory := informers.NewSharedInformerFactoryWithOptions(m.cs, 0,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", m.node).String()
		}))
	pods := factory.Core().V1().Pods()
	inf := pods.Informer()
	m.lister = pods.Lister()
	// The Node authorizer lets a node list and watch every Service, as the
	// kubelet does for the Service link variables. The shared factory also
	// feeds the gateway service table.
	svcs := m.shared.Core().V1().Services()
	svcInf := svcs.Informer()
	m.services = svcs.Lister()
	factory.Start(ctx.Done())
	m.shared.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), inf.HasSynced, svcInf.HasSynced) {
		return ctx.Err()
	}
	m.reconcile(ctx)
	_, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    m.onPod,
		UpdateFunc: func(_, obj any) { m.onPod(obj) },
		DeleteFunc: m.onDelete,
	})
	if err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// reconcile removes hull instances, addresses and pod dirs that no bound pod
// owns. Instances of bound pods are left for their workers to adopt.
func (m *podManager) reconcile(ctx context.Context) {
	pods, err := m.lister.List(labels.Everything())
	if err != nil {
		log.Printf("reconcile: list pods: %v", err)
		return
	}
	want := map[string]bool{}
	uids := map[string]bool{}
	for _, p := range pods {
		want[podvm.InstanceName(p, m.maxName)] = true
		uids[string(p.UID)] = true
	}
	list, err := m.hull.Ps(ctx)
	if err != nil {
		log.Printf("reconcile: hull ps: %v", err)
	}
	for _, in := range list {
		if want[in.ID] {
			log.Printf("reconcile: instance %s (%s) belongs to a bound pod; adopting", in.ID, in.Status)
			continue
		}
		log.Printf("reconcile: removing orphan instance %s (%s)", in.ID, in.Status)
		if err := m.hull.StopAndRemove(ctx, in.ID, 5); err != nil {
			log.Printf("reconcile: remove %s: %v", in.ID, err)
		}
	}
	if gone, err := m.ipam.Retain(uids); err != nil {
		log.Printf("reconcile: ipam: %v", err)
	} else {
		for _, a := range gone {
			log.Printf("reconcile: released %s of %s", a.Addr, a.Pod)
		}
	}
	entries, _ := os.ReadDir(m.podsDir)
	for _, e := range entries {
		if e.IsDir() && !uids[e.Name()] {
			if st, err := loadPodState(m.podsDir, e.Name()); err == nil && st != nil {
				removeForwards(ctx, m.gw, st)
			}
			_ = os.RemoveAll(filepath.Join(m.podsDir, e.Name()))
		}
	}
}

// serviceEnv returns the Service link variables for a container of the pod
// that starts now.
func (m *podManager) serviceEnv(pod *corev1.Pod) []podvm.EnvVar {
	var svcs []*corev1.Service
	if m.services != nil {
		svcs, _ = m.services.List(labels.Everything())
	}
	have := false
	for _, s := range svcs {
		if s.Namespace == "default" && s.Name == "kubernetes" {
			have = true
		}
	}
	if !have {
		svcs = append(svcs, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "kubernetes"},
			Spec: corev1.ServiceSpec{ClusterIP: m.apiServiceIP, Ports: []corev1.ServicePort{
				{Name: "https", Port: 443, Protocol: corev1.ProtocolTCP},
			}},
		})
	}
	return podvm.ServiceEnv(pod, svcs)
}

func (m *podManager) onPod(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.workers[pod.UID]
	if w == nil {
		w = newPodWorker(m, pod)
		m.workers[pod.UID] = w
		go w.loop(m.ctx)
	}
	w.update(pod, false)
}

func (m *podManager) onDelete(obj any) {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	m.mu.Lock()
	w := m.workers[pod.UID]
	m.mu.Unlock()
	if w != nil {
		w.update(pod, true)
	}
}

func (m *podManager) forget(w *podWorker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workers[w.uid] == w {
		delete(m.workers, w.uid)
	}
}

// find returns the worker of the pod ns/name, if one runs.
func (m *podManager) find(ns, name string) *podWorker {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.workers {
		if w.ns == ns && w.name == name {
			return w
		}
	}
	return nil
}

// boundPods lists the pods bound to this node from the informer cache.
func (m *podManager) boundPods() []corev1.Pod {
	if m.lister == nil {
		return nil
	}
	pods, _ := m.lister.List(labels.Everything())
	out := make([]corev1.Pod, 0, len(pods))
	for _, p := range pods {
		out = append(out, *p)
	}
	return out
}

// podWorker owns one pod: it boots, watches, restarts and tears down its VM
// and writes its status. Only the worker goroutine changes st.
type podWorker struct {
	m    *podManager
	uid  types.UID
	ns   string
	name string
	wake chan struct{}

	// Owned by the worker goroutine.
	injectSA  bool // the agent adds the service account token volume
	decidedSA bool
	lastMeta  string // labels and annotations the volumes were written for

	mu           sync.Mutex
	pod          *corev1.Pod
	gone         bool
	st           podState
	volumesDirty bool
}

func newPodWorker(m *podManager, pod *corev1.Pod) *podWorker {
	return &podWorker{m: m, uid: pod.UID, ns: pod.Namespace, name: pod.Name, pod: pod, wake: make(chan struct{}, 1)}
}

func (w *podWorker) update(pod *corev1.Pod, gone bool) {
	w.mu.Lock()
	w.pod = pod
	w.gone = w.gone || gone
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *podWorker) current() (*corev1.Pod, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pod, w.gone
}

// stopping reports whether the pod is being deleted or is gone.
func (w *podWorker) stopping() bool {
	pod, gone := w.current()
	return gone || pod.DeletionTimestamp != nil
}

// snapshot returns a copy of the state for readers outside the worker.
func (w *podWorker) snapshot() podState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.st.clone()
}

// set applies f to the state under the lock and persists it.
func (w *podWorker) set(f func(st *podState)) {
	w.mu.Lock()
	f(&w.st)
	st := w.st.clone()
	w.mu.Unlock()
	if err := savePodState(w.m.podsDir, &st); err != nil {
		log.Printf("pod %s/%s: save state: %v", w.ns, w.name, err)
	}
}

func (w *podWorker) logf(format string, args ...any) {
	log.Printf("pod %s/%s: %s", w.ns, w.name, fmt.Sprintf(format, args...))
}

func (w *podWorker) event(typ, reason, format string, args ...any) {
	pod, _ := w.current()
	w.m.recorder.Eventf(pod, typ, reason, format, args...)
}

// sleep waits for d, a pod update, or the end of ctx. It reports whether
// the pod started stopping.
func (w *podWorker) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return true
		case <-t.C:
			return w.stopping()
		case <-w.wake:
			if w.stopping() {
				return true
			}
		}
	}
}

// nap waits for d, any wake-up, or the end of ctx. It reports whether the
// pod started stopping.
func (w *podWorker) nap(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
	case <-w.wake:
	}
	return w.stopping()
}

func (w *podWorker) loop(ctx context.Context) {
	defer w.m.forget(w)
	pod, _ := w.current()
	c := &pod.Spec.Containers[0]
	w.init(ctx, pod)

	if w.st.Rejected == "" {
		if why := podvm.Validate(pod); why != "" {
			w.set(func(st *podState) { st.Rejected = why; st.Done = true })
			w.event(corev1.EventTypeWarning, podvm.ReasonUnsupported, "%s", why)
			w.logf("rejected: %s", why)
		} else if !w.st.Created {
			for _, msg := range podvm.Ignored(c) {
				w.event(corev1.EventTypeWarning, podvm.ReasonUnsupported, "%s", msg)
			}
		}
	}
	if st := w.snapshot(); st.Running && !st.Done {
		w.adopt(ctx)
	}
	w.syncStatus(ctx)

	backoff := backoffInitial
	for ctx.Err() == nil {
		if w.stopping() {
			w.teardown(ctx)
			return
		}
		if w.snapshot().Done {
			w.sleep(ctx, time.Hour)
			continue
		}
		if !w.snapshot().Running {
			if err := w.start(ctx); err != nil {
				if w.stopping() || ctx.Err() != nil {
					continue
				}
				w.logf("start: %v; retrying in %s", err, backoff)
				w.syncStatus(ctx)
				w.sleep(ctx, backoff)
				backoff = min(backoff*2, backoffMax)
				continue
			}
			w.syncStatus(ctx)
		}

		code, ok := w.watch(ctx)
		if !ok {
			continue // stopping, or the agent is shutting down
		}
		st := w.snapshot()
		now := time.Now()
		term := &termState{ExitCode: code, StartedAt: st.StartedAt, FinishedAt: now, Reason: "Completed"}
		if code != 0 {
			term.Reason = "Error"
		}
		w.logf("VM %s ended; reporting exit code %d", st.Instance, code)
		pod, _ = w.current()
		restart := pod.Spec.RestartPolicy == corev1.RestartPolicyAlways ||
			(pod.Spec.RestartPolicy == corev1.RestartPolicyOnFailure && code != 0)
		if !restart {
			w.set(func(st *podState) { st.Running = false; st.Done = true; st.Term = term })
			w.syncStatus(ctx)
			continue
		}
		if now.Sub(st.StartedAt) > backoffReset {
			backoff = backoffInitial
		}
		w.set(func(st *podState) {
			st.Running = false
			st.Term = term
			st.Waiting = &waitingState{Reason: "CrashLoopBackOff",
				Message: fmt.Sprintf("back-off %s restarting failed container=%s pod=%s_%s(%s)", backoff, c.Name, w.name, w.ns, w.uid)}
		})
		w.event(corev1.EventTypeWarning, "BackOff", "Back-off restarting failed container %s in pod %s_%s(%s)", c.Name, w.name, w.ns, w.uid)
		w.syncStatus(ctx)
		if w.sleep(ctx, backoff) {
			continue
		}
		backoff = min(backoff*2, backoffMax)
		w.set(func(st *podState) { st.RestartCount++; st.LastTerm = term; st.Term = nil })
	}
}

// adopt picks up a container that runs from before an agent restart: the
// volume subscriptions and the gateway forwards. The volumes are rewritten
// on the first turn of watch.
func (w *podWorker) adopt(ctx context.Context) {
	pod, _ := w.current()
	pod = w.effectivePod(ctx, pod)
	w.m.sources.Set(w.uid, volumeKeys(pod), w.volumesChanged)
	w.ensureForwards(ctx, &pod.Spec.Containers[0])
}

// init loads the persisted state or starts a fresh one, and decides what a
// restarted agent does with a pod it already knew.
func (w *podWorker) init(ctx context.Context, pod *corev1.Pod) {
	prev, err := loadPodState(w.m.podsDir, string(pod.UID))
	if err != nil {
		w.logf("load state: %v", err)
	}
	st := podState{
		UID: string(pod.UID), Namespace: pod.Namespace, Name: pod.Name,
		Container: pod.Spec.Containers[0].Name,
		Instance:  podvm.InstanceName(pod, w.m.maxName),
		Image:     pod.Spec.Containers[0].Image,
	}
	switch {
	case prev != nil:
		st = *prev
		if st.Running {
			in, err := w.m.hull.Inspect(ctx, st.Instance)
			if err == nil && in.Alive() {
				w.logf("adopted running VM %s (pid %d)", st.Instance, in.PID)
			} else {
				// watch reports the exit on its first poll.
				w.logf("VM %s of a running container is gone", st.Instance)
			}
		}
	case pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed:
		// Finished before this agent kept state for it: never run it again.
		st.Done = true
	}
	w.mu.Lock()
	w.st = st
	w.mu.Unlock()
}

// start boots the pod's VM: address, image, volumes, env and argv, hull
// run, then the hostname and the gateway forwards.
func (w *podWorker) start(ctx context.Context) error {
	pod, _ := w.current()
	pod = w.effectivePod(ctx, pod)
	c := &pod.Spec.Containers[0]
	h := w.m.hull
	st := w.snapshot()

	waiting := func(reason, msg string) {
		w.set(func(st *podState) { st.Waiting = &waitingState{Reason: reason, Message: msg} })
	}
	configErr := func(err error) error {
		waiting("CreateContainerConfigError", err.Error())
		w.event(corev1.EventTypeWarning, "Failed", "Error: %v", err)
		return err
	}
	ip, err := w.m.ipam.Allocate(string(pod.UID), pod.Namespace+"/"+pod.Name)
	if err != nil {
		waiting("CreateContainerError", err.Error())
		return err
	}
	w.set(func(st *podState) { st.IP = ip })

	// Volumes come first, as in the kubelet: a pod whose ConfigMap is
	// missing stays ContainerCreating with FailedMount events.
	if err := w.writeVolumes(ctx, pod); err != nil {
		waiting("ContainerCreating", err.Error())
		w.event(corev1.EventTypeWarning, "FailedMount", "%v", err)
		return err
	}
	w.lastMeta = metaKey(pod)

	img, err := w.ensureImage(ctx, c)
	if err != nil {
		return err
	}
	cfg, err := h.Config(img.Digest)
	if err != nil {
		waiting("CreateContainerConfigError", err.Error())
		return err
	}
	env, err := podvm.Env(ctx, podvm.EnvInput{
		Pod: pod, Container: c, Image: cfg, Facts: w.facts(pod),
		Services: w.m.serviceEnv(pod), Src: w.m.src,
	})
	if err != nil {
		return configErr(err)
	}
	pairs := podvm.Pairs(env)
	argv, err := podvm.Argv(c, cfg, pairs)
	if err != nil {
		return configErr(err)
	}
	plain, secret, err := podvm.HullEnv(env)
	if err != nil {
		return configErr(err)
	}
	cpus, mem := podvm.Resources(c)

	// No restart in hull: a previous VM under the name is removed first.
	if err := h.Rm(ctx, st.Instance, true); err != nil && !errors.Is(err, hull.ErrNotFound) {
		w.logf("rm stale %s: %v", st.Instance, err)
	}
	shares, err := podvm.Shares(pod, c, w.podDirs(), env)
	if err != nil {
		return configErr(err)
	}
	spec := hull.RunSpec{
		Name:        st.Instance,
		Image:       c.Image,
		Pull:        "missing", // ensureImage applied imagePullPolicy already
		MemMB:       mem,
		CPUs:        cpus,
		GatewaySock: w.m.gwSock,
		GatewayCIDR: fmt.Sprintf("%s/%d", ip, w.m.ipam.Bits()),
		StopGrace:   podvm.GraceSeconds(pod),
		Env:         plain,
		SecretEnv:   secret,
		Shares:      shares,
		Argv:        argv,
	}
	var shareArgs []string
	for _, sh := range shares {
		shareArgs = append(shareArgs, sh.Arg())
	}
	w.logf("hull run %s: %d vCPU, %d MB, %s, %d env (%d from Secrets), shares %q, argv %q",
		st.Instance, cpus, mem, spec.GatewayCIDR, len(env), len(secret), shareArgs, argv)
	t0 := time.Now()
	rctx, cancel := context.WithTimeout(ctx, runTimeout)
	_, err = h.Run(rctx, spec)
	cancel()
	if err != nil {
		waiting("RunContainerError", err.Error())
		w.event(corev1.EventTypeWarning, "Failed", "Error: %v", err)
		_ = h.Rm(ctx, st.Instance, true)
		return err
	}
	started := time.Now()
	w.set(func(st *podState) { st.Created = true; st.Env = pairs; st.ImageID = imageID(c.Image, img) })
	w.event(corev1.EventTypeNormal, "Created", "Created container: %s", c.Name)
	w.logf("VM %s up in %.1fs", st.Instance, time.Since(t0).Seconds())
	w.setHostname(ctx, pod, ip)
	w.ensureForwards(ctx, c)
	w.set(func(st *podState) {
		st.Running = true
		st.StartedAt = started
		st.Waiting = nil
		st.Term = nil
		initialProbeState(c, st)
	})
	w.event(corev1.EventTypeNormal, "Started", "Started container %s", c.Name)
	return nil
}

// metaKey fingerprints what a downwardAPI volume can read from the pod
// metadata that changes while the pod runs. fmt prints maps sorted.
func metaKey(pod *corev1.Pod) string {
	return fmt.Sprint(pod.Labels, pod.Annotations)
}

// ensureImage applies imagePullPolicy and returns the image record.
func (w *podWorker) ensureImage(ctx context.Context, c *corev1.Container) (*hull.Image, error) {
	h := w.m.hull
	w.m.pullMu.Lock()
	defer w.m.pullMu.Unlock()
	img, local := h.LocalImage(c.Image)
	policy := podvm.PullPolicy(c)
	switch {
	case policy == "never" && !local:
		msg := fmt.Sprintf("Container image %q is not present with pull policy of Never", c.Image)
		w.set(func(st *podState) { st.Waiting = &waitingState{Reason: "ErrImageNeverPull", Message: msg} })
		w.event(corev1.EventTypeWarning, "ErrImageNeverPull", "%s", msg)
		return nil, errors.New(msg)
	case local && policy != "always":
		w.event(corev1.EventTypeNormal, "Pulled", "Container image %q already present on machine", c.Image)
		return img, nil
	}
	w.event(corev1.EventTypeNormal, "Pulling", "Pulling image %q", c.Image)
	t0 := time.Now()
	digest, err := h.Pull(ctx, c.Image)
	if err == nil {
		img, err = h.ImageByDigest(digest)
	}
	if err != nil {
		w.set(func(st *podState) { st.Waiting = &waitingState{Reason: "ErrImagePull", Message: err.Error()} })
		w.event(corev1.EventTypeWarning, "Failed", "Failed to pull image %q: %v", c.Image, err)
		return nil, err
	}
	w.event(corev1.EventTypeNormal, "Pulled", "Successfully pulled image %q in %s", c.Image, time.Since(t0).Round(time.Millisecond))
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

// setHostname gives the guest the pod's hostname, which Kubernetes expects
// to equal the pod name, an /etc/hosts entry for it, and the resolv.conf of
// its dnsPolicy. It needs a shell in the image; without one the guest keeps
// hull's defaults.
func (w *podWorker) setHostname(ctx context.Context, pod *corev1.Pod, ip string) {
	name := pod.Name
	if pod.Spec.Hostname != "" {
		name = pod.Spec.Hostname
	}
	if len(name) > 63 {
		name = name[:63]
	}
	// hull's guest /dev has no stdin, stdout, stderr or fd links. A process
	// that opens /dev/stdout before this runs creates a regular file there;
	// one that starts later gets the console. The resolv.conf rewrite has
	// the same race: a lookup in the container's first moments still goes
	// to the gateway resolver hull configured.
	script := `hostname "$1" && echo "$1" > /etc/hostname && printf '%s\t%s\n' "$2" "$1" >> /etc/hosts
rc=$?
if [ -n "$3" ]; then printf '%s' "$3" > /etc/resolv.conf || rc=1; fi
for n in 0:stdin 1:stdout 2:stderr; do [ -e "/dev/${n#*:}" ] || ln -s "/proc/self/fd/${n%%:*}" "/dev/${n#*:}"; done
[ -e /dev/fd ] || ln -s /proc/self/fd /dev/fd
exit $rc`
	argv := []string{"/bin/sh", "-c", script, "sh", name, ip, podvm.ResolvConf(pod, w.m.dns)}
	deadline := time.Now().Add(hostnameWait)
	for {
		var out bytes.Buffer
		ectx, cancel := context.WithTimeout(ctx, 10*time.Second)
		code, err := w.m.hull.Exec(ectx, w.snapshot().Instance, nil, argv, nil, &out, &out)
		cancel()
		switch {
		case err == nil && code == 0:
			return
		case err == nil:
			w.logf("set hostname: exit %d: %s (does the image have /bin/sh?)", code, strings.TrimSpace(out.String()))
			return
		case strings.Contains(err.Error(), "fork/exec /bin/sh"):
			// The agent answered: the image has no shell (distroless,
			// CoreDNS). Retrying cannot help.
			w.logf("set hostname: the image has no /bin/sh; the guest keeps hull's hostname and resolv.conf")
			return
		case time.Now().After(deadline) || ctx.Err() != nil:
			w.logf("set hostname: %v", err)
			return
		}
		if in, ierr := w.m.hull.Inspect(ctx, w.snapshot().Instance); ierr != nil || !in.Alive() {
			w.logf("set hostname: the VM ended before its agent answered")
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// watch polls the VMM every pollInterval until it exits, runs the probes
// when they are due, and keeps the volumes current. It returns the exit code
// briglet reports for the container: the VM's, or 137 when a startup or
// liveness probe failed and the agent stopped the VM. ok is false when the
// pod started stopping or ctx ended.
func (w *podWorker) watch(ctx context.Context) (code int32, ok bool) {
	nextInspect := time.Now()
	for {
		if w.stopping() || ctx.Err() != nil {
			return 0, false
		}
		pod, _ := w.current()
		pod = w.effectivePod(ctx, pod)
		if now := time.Now(); !now.Before(nextInspect) {
			id := w.snapshot().Instance
			in, err := w.m.hull.Inspect(ctx, id)
			switch {
			case errors.Is(err, hull.ErrNotFound):
				return exitCodeKilled, true
			case err != nil:
				w.logf("inspect %s: %v", id, err)
			case !in.Alive():
				return w.exitCode(ctx, id), true
			}
			nextInspect = now.Add(pollInterval)
			w.gcVolumes(pod)
		}
		w.maybeRefreshVolumes(ctx, pod)
		kill, changed, next := w.probe(ctx, pod, time.Now())
		if changed {
			w.syncStatus(ctx)
		}
		if kill != "" {
			w.kill(ctx, pod, kill)
			return exitCodeKilled, true
		}
		wait := time.Until(nextInspect)
		if !next.IsZero() {
			wait = min(wait, time.Until(next))
		}
		if tr := w.snapshot().TokenRefreshAt; !tr.IsZero() {
			wait = min(wait, time.Until(tr))
		}
		if w.nap(ctx, max(wait, 100*time.Millisecond)) {
			return 0, false
		}
	}
}

// kill stops the VM of a container whose startup or liveness probe failed,
// with the probe's terminationGracePeriodSeconds or the pod's.
func (w *podWorker) kill(ctx context.Context, pod *corev1.Pod, kind string) {
	c := &pod.Spec.Containers[0]
	grace := podvm.GraceSeconds(pod)
	for _, ps := range probesOf(c) {
		if ps.kind == kind && ps.p.TerminationGracePeriodSeconds != nil {
			grace = int(*ps.p.TerminationGracePeriodSeconds)
		}
	}
	id := w.snapshot().Instance
	t0 := time.Now()
	sctx, cancel := context.WithTimeout(ctx, time.Duration(grace+60)*time.Second)
	defer cancel()
	if err := w.m.hull.Stop(sctx, id, grace); err != nil {
		w.logf("stop %s after the %s probe failed: %v", id, kind, err)
	}
	w.logf("VM %s stopped in %.1fs after the %s probe failed", id, time.Since(t0).Seconds(), kind)
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

// exitCode reads the end of the console log: a guest that halted or
// rebooted itself counts as a clean exit.
func (w *podWorker) exitCode(ctx context.Context, id string) int32 {
	var buf bytes.Buffer
	if err := w.m.hull.Logs(ctx, id, false, 20, &buf); err != nil {
		return exitCodeKilled
	}
	if strings.Contains(buf.String(), "guest stopped:") || strings.Contains(buf.String(), "reboot: ") {
		return exitCodeClean
	}
	return exitCodeKilled
}

// teardown stops and removes the VM, frees the address and, when the pod
// object still exists, deletes it with grace 0 as the kubelet does.
func (w *podWorker) teardown(ctx context.Context) {
	pod, gone := w.current()
	st := w.snapshot()
	grace := podvm.GraceSeconds(pod)
	if gone {
		grace = 2
	}
	if st.Created || st.Running {
		w.event(corev1.EventTypeNormal, "Killing", "Stopping container %s", st.Container)
		t0 := time.Now()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(grace+60)*time.Second)
		err := w.m.hull.StopAndRemove(sctx, st.Instance, grace)
		cancel()
		if err != nil {
			w.logf("teardown %s: %v", st.Instance, err)
		} else {
			w.logf("VM %s stopped and removed in %.1fs", st.Instance, time.Since(t0).Seconds())
		}
	}
	w.m.sources.Release(w.uid)
	w.m.src.forget(w.uid)
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	removeForwards(fctx, w.m.gw, &st)
	fcancel()
	if err := w.m.ipam.Release(string(w.uid)); err != nil {
		w.logf("release address: %v", err)
	}
	_ = os.RemoveAll(podDir(w.m.podsDir, string(w.uid)))
	if gone {
		return
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := w.m.cs.CoreV1().Pods(w.ns).Delete(dctx, w.name, metav1.DeleteOptions{
		GracePeriodSeconds: ptr.To[int64](0),
		Preconditions:      &metav1.Preconditions{UID: &w.uid},
	})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		w.logf("delete: %v", err)
	}
}
