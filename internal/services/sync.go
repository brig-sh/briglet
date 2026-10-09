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

package services

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/brig-sh/briglet/internal/hull"
)

const (
	// debounce gathers a burst of Service and EndpointSlice events into
	// one table.
	debounce = 200 * time.Millisecond
	// resync compares the gateway's table and forwards with the wanted
	// ones, so a gateway that restarted gets them back.
	resync     = 30 * time.Second
	retryAfter = 2 * time.Second
)

// Syncer keeps the gateway's service table and NodePort forwards equal to
// what the Services and EndpointSlices ask for.
type Syncer struct {
	gw     *hull.Gateway
	p      Params
	debug  bool
	svcs   corelisters.ServiceLister
	slices discoverylisters.EndpointSliceLister
	synced []cache.InformerSynced
	kick   chan struct{}
	// installed is the last table the gateway accepted.
	installed []hull.Service
}

// New registers the Service and EndpointSlice informers on factory. The
// caller starts the factory.
func New(factory informers.SharedInformerFactory, gw *hull.Gateway, p Params, debug bool) (*Syncer, error) {
	s := &Syncer{gw: gw, p: p, debug: debug, kick: make(chan struct{}, 1)}
	svcInf := factory.Core().V1().Services()
	sliceInf := factory.Discovery().V1().EndpointSlices()
	s.svcs = svcInf.Lister()
	s.slices = sliceInf.Lister()
	h := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { s.trigger() },
		UpdateFunc: func(_, _ any) { s.trigger() },
		DeleteFunc: func(any) { s.trigger() },
	}
	for _, inf := range []cache.SharedIndexInformer{svcInf.Informer(), sliceInf.Informer()} {
		if _, err := inf.AddEventHandler(h); err != nil {
			return nil, err
		}
		s.synced = append(s.synced, inf.HasSynced)
	}
	return s, nil
}

func (s *Syncer) trigger() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run syncs the gateway until ctx ends.
func (s *Syncer) Run(ctx context.Context) error {
	if !cache.WaitForCacheSync(ctx.Done(), s.synced...) {
		return ctx.Err()
	}
	log.Printf("services: informers synced; service CIDR %s", s.p.ServiceCIDR)
	tick := time.NewTicker(resync)
	defer tick.Stop()
	var timer <-chan time.Time
	s.trigger()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.kick:
			// Every event in the window restarts it.
			timer = time.After(debounce)
		case <-timer:
			timer = nil
			if !s.sync(ctx, false) {
				timer = time.After(retryAfter)
			}
		case <-tick.C:
			if !s.sync(ctx, true) {
				timer = time.After(retryAfter)
			}
		}
	}
}

// sync installs the table and reconciles the NodePort forwards. With check
// it reads the gateway's table and puts it only when it differs. It
// reports whether everything went through.
func (s *Syncer) sync(ctx context.Context, check bool) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	svcs, err := s.svcs.List(labels.Everything())
	if err != nil {
		log.Printf("services: list services: %v", err)
		return false
	}
	slices, err := s.slices.List(labels.Everything())
	if err != nil {
		log.Printf("services: list endpointslices: %v", err)
		return false
	}
	table, skipped := Build(svcs, slices, s.p)
	for _, sk := range skipped {
		log.Printf("services: skipping endpoint %s: not on the pod network or the gateway", sk)
	}
	put := !reflect.DeepEqual(table, s.installed)
	if check {
		have, err := s.gw.Services(ctx)
		if err != nil {
			log.Printf("services: read gateway table: %v", err)
			return false
		}
		if !equalTables(have, table) {
			log.Printf("services: gateway table differs from the wanted one; putting it again")
			put = true
		}
	}
	ok := true
	if put {
		if err := s.gw.PutServices(ctx, table); err != nil {
			log.Printf("services: put table: %v", err)
			ok = false
		} else {
			s.installed = table
			n := 0
			for _, e := range table {
				n += len(e.Endpoints)
			}
			log.Printf("services: installed %d entries with %d endpoints", len(table), n)
			if s.debug {
				b, _ := json.Marshal(table)
				log.Printf("services: table %s", b)
			}
		}
	}
	if !s.reconcileNodePorts(ctx, NodePorts(svcs, s.p)) {
		ok = false
	}
	return ok
}

// equalTables compares two tables, treating a missing endpoint list as
// empty.
func equalTables(a, b []hull.Service) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.VIP != y.VIP || x.Port != y.Port || x.Protocol != y.Protocol || len(x.Endpoints) != len(y.Endpoints) {
			return false
		}
		for j := range x.Endpoints {
			if x.Endpoints[j] != y.Endpoints[j] {
				return false
			}
		}
	}
	return true
}

// ownedForward reports whether a gateway forward is a NodePort forward:
// a port in the node port range carried to a cluster IP. The agent's
// per-pod forwards use ephemeral ports and pod addresses.
func (s *Syncer) ownedForward(f hull.Forward) (port int, ok bool) {
	_, lp, err := net.SplitHostPort(f.Local)
	if err != nil {
		return 0, false
	}
	port, err = strconv.Atoi(lp)
	if err != nil || port < NodePortMin || port > NodePortMax {
		return 0, false
	}
	rh, _, err := net.SplitHostPort(f.Remote)
	if err != nil {
		return 0, false
	}
	ip, err := netip.ParseAddr(rh)
	if err != nil || !s.p.ServiceCIDR.Contains(ip) {
		return 0, false
	}
	return port, true
}

// reconcileNodePorts makes the gateway forward each node port to its
// cluster IP and withdraws node port forwards no Service asks for.
func (s *Syncer) reconcileNodePorts(ctx context.Context, want []NodePort) bool {
	have, err := s.gw.Forwards(ctx)
	if err != nil {
		log.Printf("services: list forwards: %v", err)
		return false
	}
	type key struct {
		proto string
		port  int32
	}
	wanted := map[key]NodePort{}
	for _, np := range want {
		wanted[key{np.Protocol, np.Port}] = np
	}
	ok := true
	present := map[key]bool{}
	for _, f := range have {
		port, mine := s.ownedForward(f)
		if !mine {
			continue
		}
		k := key{f.Protocol, int32(port)}
		if np, keep := wanted[k]; keep && np.Remote == f.Remote && !present[k] {
			present[k] = true
			continue
		}
		if err := s.gw.RemoveForward(ctx, f.Protocol, f.Local); err != nil {
			log.Printf("services: withdraw node port %s %s: %v", f.Protocol, f.Local, err)
			ok = false
			continue
		}
		log.Printf("services: withdrew node port %s %s -> %s", f.Protocol, f.Local, f.Remote)
	}
	for _, np := range want {
		k := key{np.Protocol, np.Port}
		if present[k] {
			continue
		}
		port := strconv.Itoa(int(np.Port))
		local := net.JoinHostPort("0.0.0.0", port)
		err := s.gw.AddForward(ctx, hull.Forward{Protocol: np.Protocol, Local: local, Remote: np.Remote})
		if err != nil {
			log.Printf("services: node port on %s: %v; trying 127.0.0.1", local, err)
			local = net.JoinHostPort("127.0.0.1", port)
			err = s.gw.AddForward(ctx, hull.Forward{Protocol: np.Protocol, Local: local, Remote: np.Remote})
		}
		if err != nil {
			log.Printf("services: node port %s %s -> %s: %v", np.Protocol, port, np.Remote, err)
			ok = false
			continue
		}
		present[k] = true
		log.Printf("services: node port %s %s -> %s", np.Protocol, local, np.Remote)
	}
	return ok
}
