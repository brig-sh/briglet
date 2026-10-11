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
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

	"github.com/brig-sh/briglet/internal/guest"
	"github.com/brig-sh/briglet/internal/hull"
	"github.com/brig-sh/briglet/internal/podvm"
)

const (
	// pollInterval is how often the agent checks that the VM still runs;
	// guestPoll is how often it reads status.json.
	pollInterval   = 5 * time.Second
	guestPoll      = 250 * time.Millisecond
	backoffInitial = 10 * time.Second
	backoffMax     = 5 * time.Minute
	// A VM that ran this long without ending resets the back-off.
	backoffReset = 10 * time.Minute
	runTimeout   = 5 * time.Minute
	// supervisorWait bounds the time from boot to the first status.json.
	supervisorWait = 60 * time.Second
	// exitCodeKilled is what a container that was running reports when its
	// VM went away underneath it.
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
	dns podvm.DNS
	gw  *hull.Gateway
	// guestBinDir holds briglet-guest; every VM shares it read-only.
	guestBinDir string
	// baseImage is the image every pod VM boots.
	baseImage string
	// registryMirror, when set, serves every Docker Hub image.
	registryMirror string
	// overlayBlock boots pods from block devices; see Config.RootfsMode.
	overlayBlock bool
	// imageLinks holds a colon-free symlink to each image rootfs, since
	// hull's share syntax splits on colons and digests carry one.
	imageLinks  string
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
		apiServiceIP:   apiIP,
		shared:         shared,
		dns:            podvm.DNS{ClusterDNS: cfg.ClusterDNS, Domain: cfg.ClusterDomain, Upstream: cfg.HostIP},
		gw:             hull.NewGateway(cfg.GatewaySock + ".api"),
		guestBinDir:    cfg.GuestBinDir,
		baseImage:      cfg.BaseImage,
		registryMirror: cfg.RegistryMirror,
		overlayBlock:   cfg.RootfsMode == RootfsOverlayBlock,
		imageLinks:     filepath.Join(cfg.StateDir, "image-links"),
		sources:        sw,
		src:            &apiSources{cs: cs, watch: sw},
		probeClient:    newProbeClient(),
		cs:             cs,
		node:           cfg.Name,
		hostIP:         cfg.HostIP,
		hull:           h,
		ipam:           ipam,
		podsDir:        cfg.PodsDir,
		gwSock:         cfg.GatewaySock,
		maxName:        maxName,
		recorder:       b.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "kubelet", Host: cfg.Name}),
		workers:        map[types.UID]*podWorker{},
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

// reasonNodePorts is the status reason of a pod refused for its hostPort.
const reasonNodePorts = "NodePorts"

// hostPortConflict reports why pod cannot take its hostPorts: another pod
// of this node that holds a VM already uses one of them. It returns "" when
// there is no conflict.
func (m *podManager) hostPortConflict(self *podWorker, pod *corev1.Pod) string {
	if len(hostPortsOf(pod)) == 0 {
		return ""
	}
	m.mu.Lock()
	var others []*podWorker
	for _, w := range m.workers {
		if w != self {
			others = append(others, w)
		}
	}
	m.mu.Unlock()
	for _, o := range others {
		st := o.snapshot()
		if !st.Running || st.Done {
			continue
		}
		if op, _ := o.current(); op != nil && hostPortsConflict(pod, op) {
			return "Pod was rejected: node(s) didn't have free ports for the requested pod ports"
		}
	}
	return ""
}

// hostPortsOf lists the pod's container ports that name a host port.
func hostPortsOf(pod *corev1.Pod) []corev1.ContainerPort {
	var out []corev1.ContainerPort
	for _, c := range podvm.AllContainers(pod) {
		for _, p := range c.Ports {
			if p.HostPort > 0 {
				if p.Protocol == "" {
					p.Protocol = corev1.ProtocolTCP
				}
				out = append(out, p)
			}
		}
	}
	return out
}

// hostPortsConflict reports whether a and b ask for the same host port with
// the same protocol on overlapping host addresses; an empty or unspecified
// hostIP covers every address.
func hostPortsConflict(a, b *corev1.Pod) bool {
	wild := func(ip string) bool { return ip == "" || ip == "0.0.0.0" || ip == "::" }
	for _, pa := range hostPortsOf(a) {
		for _, pb := range hostPortsOf(b) {
			if pa.HostPort == pb.HostPort && pa.Protocol == pb.Protocol &&
				(wild(pa.HostIP) || wild(pb.HostIP) || pa.HostIP == pb.HostIP) {
				return true
			}
		}
	}
	return false
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
	obsGen       atomic.Int64 // generation the last status write observed
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
// deadlinePassed reports whether the pod's activeDeadlineSeconds elapsed
// while it still runs.
func (w *podWorker) deadlinePassed() bool {
	pod, _ := w.current()
	if pod == nil || pod.Spec.ActiveDeadlineSeconds == nil || pod.CreationTimestamp.IsZero() {
		return false
	}
	if w.snapshot().Done || !w.snapshot().Running {
		return false
	}
	return time.Since(pod.CreationTimestamp.Time) > time.Duration(*pod.Spec.ActiveDeadlineSeconds)*time.Second
}

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
			w.syncGeneration(ctx)
		}
	}
}

// syncGeneration writes the status when the pod's spec changed since the
// last write, so status.observedGeneration follows metadata.generation even
// while the pod is idle, finished or waiting to start.
func (w *podWorker) syncGeneration(ctx context.Context) {
	if pod, gone := w.current(); !gone && pod.Generation != w.obsGen.Load() {
		w.syncStatus(ctx)
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
		if !w.stopping() {
			w.syncGeneration(ctx)
		}
	}
	return w.stopping()
}

func (w *podWorker) loop(ctx context.Context) {
	defer w.m.forget(w)
	pod, _ := w.current()
	w.init(ctx, pod)

	if w.st.Rejected == "" {
		if why := podvm.Validate(pod); why != "" {
			w.set(func(st *podState) { st.Rejected = why; st.Done = true; st.Phase = string(corev1.PodFailed) })
			w.event(corev1.EventTypeWarning, podvm.ReasonUnsupported, "%s", why)
			w.logf("rejected: %s", why)
		} else if why := w.m.hostPortConflict(w, pod); why != "" && !w.st.Created {
			// The node's port space is a Mac's: like the kubelet's predicate
			// admission, refuse a pod whose hostPort a running pod holds.
			w.set(func(st *podState) {
				st.Rejected, st.RejectedReason = why, reasonNodePorts
				st.Done, st.Phase = true, string(corev1.PodFailed)
			})
			w.event(corev1.EventTypeWarning, reasonNodePorts, "%s", why)
			w.logf("rejected: %s", why)
		} else if !w.st.Created {
			for _, c := range podvm.AllContainers(pod) {
				for _, msg := range podvm.Ignored(c) {
					w.event(corev1.EventTypeWarning, podvm.ReasonUnsupported, "%s", msg)
				}
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
		if w.deadlinePassed() {
			w.set(func(st *podState) {
				st.Deadline = true
				st.Done = true
				st.Phase = string(corev1.PodFailed)
				st.Reason = "DeadlineExceeded"
			})
			w.event(corev1.EventTypeNormal, "DeadlineExceeded", "Pod was active longer than specified deadline")
			w.logf("activeDeadlineSeconds elapsed")
			// Stop the containers but keep the pod object, as the kubelet
			// does: the statuses carry the DeadlineExceeded reason.
			w.stopRunning(ctx)
			w.syncStatus(ctx)
			w.sleep(ctx, time.Hour)
			continue
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
		how := w.watch(ctx)
		if how == vmRunning {
			continue // stopping, or the agent is shutting down
		}
		if time.Since(w.snapshot().StartedAt) > backoffReset {
			backoff = backoffInitial
		}
		if w.vmEnded(ctx, how) {
			w.syncStatus(ctx)
			continue
		}
		w.syncStatus(ctx)
		if w.sleep(ctx, backoff) {
			continue
		}
		backoff = min(backoff*2, backoffMax)
	}
}

// adopt picks up a VM that runs from before an agent restart: the volume
// subscriptions and the gateway forwards. The volumes are rewritten on the
// first turn of watch.
func (w *podWorker) adopt(ctx context.Context) {
	pod, _ := w.current()
	pod = w.effectivePod(ctx, pod)
	w.m.sources.Set(w.uid, volumeKeys(pod), w.volumesChanged)
	w.ensureForwards(ctx, pod)
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
		Instance: podvm.InstanceName(pod, w.m.maxName),
	}
	switch {
	case prev != nil:
		st = *prev
		if st.Running {
			in, err := w.m.hull.Inspect(ctx, st.Instance)
			if err == nil && in.Alive() {
				w.logf("adopted running VM %s (pid %d)", st.Instance, in.PID)
			} else {
				// watch reports the end on its first poll.
				w.logf("VM %s of a running pod is gone", st.Instance)
			}
		}
	case pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed:
		// Finished before this agent kept state for it: never run it again.
		st.Done = true
		st.Phase = string(pod.Status.Phase)
	}
	w.mu.Lock()
	w.st = st
	w.mu.Unlock()
}

// teardown stops the pod: the supervisor gets the grace period to stop the
// containers and power the VM off, then the VM is removed. The address and
// pod dir are freed and, when the pod object still exists, it is deleted
// with grace 0 as the kubelet does.
// stopRunning stops the VM and releases its resources without deleting
// the pod object: the deadline path, whose statuses the agent keeps
// reporting.
func (w *podWorker) stopRunning(ctx context.Context) {
	st := w.snapshot()
	bg := context.WithoutCancel(ctx)
	if st.Running && w.vmAlive(bg) {
		cctx, cancel := context.WithTimeout(bg, 10*time.Second)
		err := w.ctl(cctx, guest.CtlRequest{Op: guest.CtlStop, Grace: 2})
		cancel()
		if err != nil {
			w.logf("stop through the supervisor: %v", err)
		} else if !w.waitVMEnd(bg, 10*time.Second) {
			w.logf("VM %s still runs after the stop; stopping it", st.Instance)
		}
	}
	sctx, cancel := context.WithTimeout(bg, 60*time.Second)
	if err := w.m.hull.StopAndRemove(sctx, st.Instance, 2); err != nil {
		w.logf("stop %s: %v", st.Instance, err)
	}
	cancel()
	// Keep the supervisor's final status, so the containers end with their
	// real terminations instead of ContainerStatusUnknown.
	pod, _ := w.current()
	w.readGuest(bg, pod)
	w.set(func(n *podState) { n.Running = false })
	removeForwards(bg, w.m.gw, &st)
}

// finishStatus writes the terminal status of a pod that is being deleted,
// before its object is removed, as the kubelet does: Succeeded when every
// container exited 0, Failed otherwise. A pod object that keeps a finalizer,
// such as a Job's pod, would stay in Terminating with phase Running, because
// the controller removes the finalizer only from a pod that finished.
func (w *podWorker) finishStatus(ctx context.Context) {
	pod, _ := w.current()
	w.readGuest(ctx, pod)
	w.set(func(st *podState) {
		st.Running = false
		if st.Done {
			return
		}
		st.Done, st.Phase = true, string(corev1.PodSucceeded)
		for _, c := range pod.Spec.Containers {
			if cs := st.Guest.Find(c.Name); cs == nil || cs.Term == nil || cs.Term.ExitCode != 0 {
				st.Phase = string(corev1.PodFailed)
			}
		}
	})
	w.syncStatus(ctx)
}

func (w *podWorker) teardown(ctx context.Context) {
	pod, gone := w.current()
	st := w.snapshot()
	grace := podvm.GraceSeconds(pod)
	if gone {
		grace = 2
	}
	bg := context.WithoutCancel(ctx)
	if st.Created || st.Running {
		t0 := time.Now()
		if st.Running && w.vmAlive(bg) {
			for _, cs := range runningContainers(st.Guest) {
				w.event(corev1.EventTypeNormal, "Killing", "Stopping container %s", cs)
			}
			cctx, cancel := context.WithTimeout(bg, 10*time.Second)
			err := w.ctl(cctx, guest.CtlRequest{Op: guest.CtlStop, Grace: grace})
			cancel()
			if err != nil {
				w.logf("stop through the supervisor: %v", err)
			} else if !w.waitVMEnd(bg, time.Duration(grace+5)*time.Second) {
				w.logf("VM %s still runs %d s after the stop; stopping it", st.Instance, grace+5)
			}
		}
		sctx, cancel := context.WithTimeout(bg, 60*time.Second)
		err := w.m.hull.StopAndRemove(sctx, st.Instance, 2)
		cancel()
		if err != nil {
			w.logf("teardown %s: %v", st.Instance, err)
		} else {
			w.logf("VM %s stopped and removed in %.1fs", st.Instance, time.Since(t0).Seconds())
		}
	}
	if !gone {
		w.finishStatus(bg)
	}
	w.m.sources.Release(w.uid)
	w.m.src.forget(w.uid)
	fctx, fcancel := context.WithTimeout(bg, 10*time.Second)
	removeForwards(fctx, w.m.gw, &st)
	fcancel()
	if err := w.m.ipam.Release(string(w.uid)); err != nil {
		w.logf("release address: %v", err)
	}
	_ = os.RemoveAll(podDir(w.m.podsDir, string(w.uid)))
	if gone {
		return
	}
	dctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	err := w.m.cs.CoreV1().Pods(w.ns).Delete(dctx, w.name, metav1.DeleteOptions{
		GracePeriodSeconds: ptr.To[int64](0),
		Preconditions:      &metav1.Preconditions{UID: &w.uid},
	})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		w.logf("delete: %v", err)
	}
}

// runningContainers names the containers the guest last reported running.
func runningContainers(g *guest.Status) []string {
	var out []string
	if g == nil {
		return out
	}
	for _, c := range g.Containers {
		if c.State == guest.StateRunning {
			out = append(out, c.Name)
		}
	}
	return out
}

// vmAlive reports whether the pod's VMM process runs.
func (w *podWorker) vmAlive(ctx context.Context) bool {
	in, err := w.m.hull.Inspect(ctx, w.snapshot().Instance)
	return err == nil && in.Alive()
}

// waitVMEnd waits up to d for the VM to power off.
func (w *podWorker) waitVMEnd(ctx context.Context, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !w.vmAlive(ctx) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
	return false
}
