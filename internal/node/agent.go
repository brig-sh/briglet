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

// Package node is the briglet node agent: it registers the Mac as a Node,
// keeps its Lease and status fresh, and serves the kubelet API.
package node

import (
	"context"
	"fmt"
	"log"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

	"github.com/brig-sh/briglet/internal/hull"
)

const (
	Version        = "briglet/0.0.1"
	leaseNamespace = "kube-node-lease"
	leaseDuration  = 40 * time.Second
	renewInterval  = 10 * time.Second
	statusInterval = 10 * time.Second
	defaultMaxPods = 110
	hostOSLabel    = "briglet.io/host-os"
)

// Config holds the node agent settings.
type Config struct {
	Kubeconfig   string
	Name         string
	NodeIP       string
	Port         int
	TLSCertFile  string
	TLSKeyFile   string
	ClientCAFile string
	StateDir     string
	HullBin      string
	// HostIP is reported as the pods' hostIP: the gateway address, which is
	// how a pod reaches the host.
	HostIP      string
	HullStore   string
	GatewaySock string
	PodsDir     string
	IPAMFile    string
	PodCIDR     string
	// APIServiceIP is the cluster IP of the `kubernetes` Service.
	APIServiceIP string
}

// Agent is a running node agent.
type Agent struct {
	cfg   Config
	cs    kubernetes.Interface
	facts HostFacts
	uid   string
	pods  *podManager
}

// Run registers the node and blocks until ctx is done or a loop fails.
func Run(ctx context.Context, cfg Config) error {
	rc, err := clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
	if err != nil {
		return fmt.Errorf("kubeconfig %s: %w", cfg.Kubeconfig, err)
	}
	rc.UserAgent = Version
	rc.Timeout = 10 * time.Second
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	facts, err := readHostFacts(cfg.StateDir, cfg.HullBin)
	if err != nil {
		return err
	}
	h := hull.New(cfg.HullBin, cfg.HullStore)
	pm, err := newPodManager(cs, cfg, h)
	if err != nil {
		return err
	}
	a := &Agent{cfg: cfg, cs: cs, facts: facts, pods: pm}

	errc := make(chan error, 2)
	go func() {
		if err := serveKubelet(ctx, fmt.Sprintf(":%d", cfg.Port), cfg.TLSCertFile, cfg.TLSKeyFile, cfg.ClientCAFile, cfg.Name, pm); err != nil {
			errc <- fmt.Errorf("kubelet server: %w", err)
		}
	}()

	if err := retryUntil(ctx, "register node", a.register); err != nil {
		return err
	}
	log.Printf("node %s registered (uid %s): %d cpu, %d bytes memory, os image %q", cfg.Name, a.uid, facts.CPUs, facts.MemoryBytes, facts.OSImage)

	go a.loop(ctx, "lease", renewInterval, a.renewLease)
	go a.loop(ctx, "status", statusInterval, a.updateStatus)
	go func() {
		if err := pm.run(ctx); err != nil && ctx.Err() == nil {
			errc <- fmt.Errorf("pod sync: %w", err)
		}
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

func retryUntil(ctx context.Context, what string, f func(context.Context) error) error {
	for {
		err := f(ctx)
		if err == nil {
			return nil
		}
		log.Printf("%s: %v", what, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (a *Agent) loop(ctx context.Context, name string, every time.Duration, f func(context.Context) error) {
	run := func() {
		if err := f(ctx); err != nil && ctx.Err() == nil {
			log.Printf("%s: %v", name, err)
		}
	}
	run()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

func (a *Agent) labels() map[string]string {
	return map[string]string{
		"kubernetes.io/hostname": a.cfg.Name,
		"kubernetes.io/os":       "linux",
		"kubernetes.io/arch":     "arm64",
		hostOSLabel:              "darwin",
	}
}

func (a *Agent) resources() corev1.ResourceList {
	rl := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(a.facts.CPUs), resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(a.facts.MemoryBytes, resource.BinarySI),
		corev1.ResourcePods:   *resource.NewQuantity(defaultMaxPods, resource.DecimalSI),
	}
	if a.facts.DiskBytes > 0 {
		rl[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(a.facts.DiskBytes, resource.BinarySI)
	}
	return rl
}

// fillStatus overwrites the fields of status the agent owns and refreshes
// condition heartbeats. Transition times only move when a status flips.
func (a *Agent) fillStatus(st *corev1.NodeStatus) {
	now := metav1.Now()
	st.Capacity = a.resources()
	st.Allocatable = a.resources()
	st.Addresses = []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: a.cfg.NodeIP},
		{Type: corev1.NodeHostName, Address: a.cfg.Name},
	}
	// The apiserver proxies kubectl's exec websocket to the agent as is
	// when the node declares this; otherwise it translates it to SPDY.
	st.DeclaredFeatures = []string{"ExtendWebSocketsToKubelet"}
	st.DaemonEndpoints.KubeletEndpoint.Port = int32(a.cfg.Port)
	st.NodeInfo = corev1.NodeSystemInfo{
		MachineID:               a.facts.SystemUUID,
		SystemUUID:              a.facts.SystemUUID,
		BootID:                  a.facts.BootID,
		KernelVersion:           a.facts.KernelVersion,
		OSImage:                 a.facts.OSImage,
		ContainerRuntimeVersion: a.facts.RuntimeVer,
		KubeletVersion:          Version,
		OperatingSystem:         "linux",
		Architecture:            "arm64",
	}
	want := []corev1.NodeCondition{
		{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse, Reason: "KubeletHasSufficientMemory", Message: "node agent has sufficient memory available"},
		{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse, Reason: "KubeletHasNoDiskPressure", Message: "node agent has no disk pressure"},
		{Type: corev1.NodePIDPressure, Status: corev1.ConditionFalse, Reason: "KubeletHasSufficientPID", Message: "node agent has sufficient PID available"},
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady", Message: "briglet node agent is posting ready status"},
	}
	for _, w := range want {
		w.LastHeartbeatTime = now
		w.LastTransitionTime = now
		replaced := false
		for i := range st.Conditions {
			if st.Conditions[i].Type != w.Type {
				continue
			}
			if st.Conditions[i].Status == w.Status {
				w.LastTransitionTime = st.Conditions[i].LastTransitionTime
			}
			st.Conditions[i] = w
			replaced = true
		}
		if !replaced {
			st.Conditions = append(st.Conditions, w)
		}
	}
}

func (a *Agent) register(ctx context.Context) error {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: a.cfg.Name, Labels: a.labels()}}
	a.fillStatus(&n.Status)
	created, err := a.cs.CoreV1().Nodes().Create(ctx, n, metav1.CreateOptions{})
	if err == nil {
		a.uid = string(created.UID)
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	// The Node survives a restart of the agent. Reconcile our labels onto it.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := a.cs.CoreV1().Nodes().Get(ctx, a.cfg.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if cur.Labels == nil {
			cur.Labels = map[string]string{}
		}
		for k, v := range a.labels() {
			cur.Labels[k] = v
		}
		upd, err := a.cs.CoreV1().Nodes().Update(ctx, cur, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		a.uid = string(upd.UID)
		return nil
	})
}

func (a *Agent) updateStatus(ctx context.Context) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := a.cs.CoreV1().Nodes().Get(ctx, a.cfg.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		a.fillStatus(&cur.Status)
		_, err = a.cs.CoreV1().Nodes().UpdateStatus(ctx, cur, metav1.UpdateOptions{})
		return err
	})
}

func (a *Agent) renewLease(ctx context.Context) error {
	leases := a.cs.CoordinationV1().Leases(leaseNamespace)
	now := metav1.NewMicroTime(time.Now())
	l, err := leases.Get(ctx, a.cfg.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		l = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      a.cfg.Name,
				Namespace: leaseNamespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "v1", Kind: "Node", Name: a.cfg.Name, UID: types.UID(a.uid),
				}},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr.To(a.cfg.Name),
				LeaseDurationSeconds: ptr.To(int32(leaseDuration.Seconds())),
				RenewTime:            &now,
			},
		}
		_, err = leases.Create(ctx, l, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	l.Spec.HolderIdentity = ptr.To(a.cfg.Name)
	l.Spec.LeaseDurationSeconds = ptr.To(int32(leaseDuration.Seconds()))
	l.Spec.RenewTime = &now
	_, err = leases.Update(ctx, l, metav1.UpdateOptions{})
	return err
}
