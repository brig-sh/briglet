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
	"sort"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/brig-sh/briglet/internal/podvm"
)

// Token refresh follows the kubelet's token manager: a token is refreshed
// once 80% of its lifetime has passed, and at least once a day.
const (
	tokenRefreshFraction = 0.8
	tokenMaxAge          = 24 * time.Hour
)

// watchKey names one ConfigMap or Secret.
type watchKey struct {
	Kind      string // "configmaps" or "secrets"
	Namespace string
	Name      string
}

func (k watchKey) String() string { return k.Kind + "/" + k.Namespace + "/" + k.Name }

// watchEntry is a single-object informer and the pods that read the object.
type watchEntry struct {
	stop  chan struct{}
	store cache.Store
	ctrl  cache.Controller
	subs  map[types.UID]func()
}

// sourceWatch keeps one informer per ConfigMap or Secret that a running pod
// mounts, and tells those pods when the object changes. Each informer lists
// and watches a single object by name, which is what the Node authorizer
// lets a node do for objects its pods reference. An informer stops when the
// last pod that reads its object goes.
type sourceWatch struct {
	cs kubernetes.Interface

	mu      sync.Mutex
	entries map[watchKey]*watchEntry
	byPod   map[types.UID][]watchKey
}

func newSourceWatch(cs kubernetes.Interface) *sourceWatch {
	return &sourceWatch{cs: cs, entries: map[watchKey]*watchEntry{}, byPod: map[types.UID][]watchKey{}}
}

// Set makes the pod's subscriptions exactly keys. notify runs on every
// change of one of the objects, on an informer goroutine.
func (s *sourceWatch) Set(uid types.UID, keys []watchKey, notify func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[watchKey]bool{}
	for _, k := range keys {
		want[k] = true
	}
	for _, k := range s.byPod[uid] {
		if !want[k] {
			s.unsubscribe(uid, k)
		}
	}
	for _, k := range keys {
		e := s.entries[k]
		if e == nil {
			e = s.start(k)
			s.entries[k] = e
		}
		e.subs[uid] = notify
	}
	if len(keys) == 0 {
		delete(s.byPod, uid)
	} else {
		s.byPod[uid] = keys
	}
}

// Release drops every subscription of the pod.
func (s *sourceWatch) Release(uid types.UID) { s.Set(uid, nil, nil) }

func (s *sourceWatch) unsubscribe(uid types.UID, k watchKey) {
	e := s.entries[k]
	if e == nil {
		return
	}
	delete(e.subs, uid)
	if len(e.subs) == 0 {
		close(e.stop)
		delete(s.entries, k)
		log.Printf("sources: stopped watching %s", k)
	}
}

func (s *sourceWatch) start(k watchKey) *watchEntry {
	var obj runtime.Object = &corev1.ConfigMap{}
	if k.Kind == "secrets" {
		obj = &corev1.Secret{}
	}
	lw := cache.NewFilteredListWatchFromClient(s.cs.CoreV1().RESTClient(), k.Kind, k.Namespace, func(o *metav1.ListOptions) {
		o.FieldSelector = fields.OneTermEqualSelector("metadata.name", k.Name).String()
	})
	e := &watchEntry{stop: make(chan struct{}), subs: map[types.UID]func(){}}
	changed := func(any) {
		s.mu.Lock()
		fns := make([]func(), 0, len(e.subs))
		for _, f := range e.subs {
			fns = append(fns, f)
		}
		s.mu.Unlock()
		for _, f := range fns {
			if f != nil {
				f()
			}
		}
	}
	e.store, e.ctrl = cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: lw,
		ObjectType:    obj,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    changed,
			UpdateFunc: func(_, n any) { changed(n) },
			DeleteFunc: changed,
		},
	})
	go e.ctrl.Run(e.stop)
	log.Printf("sources: watching %s", k)
	return e
}

// cached returns the object from a synced informer, if there is one.
func (s *sourceWatch) cached(k watchKey) (any, bool, bool) {
	s.mu.Lock()
	e := s.entries[k]
	s.mu.Unlock()
	if e == nil || !e.ctrl.HasSynced() {
		return nil, false, false
	}
	obj, exists, err := e.store.GetByKey(k.Namespace + "/" + k.Name)
	if err != nil {
		return nil, false, false
	}
	return obj, exists, true
}

// keys lists what the pod's volumes read, which is what Set subscribes to.
func volumeKeys(pod *corev1.Pod) []watchKey {
	cms, secs := podvm.References(pod)
	var keys []watchKey
	for _, n := range cms {
		keys = append(keys, watchKey{Kind: "configmaps", Namespace: pod.Namespace, Name: n})
	}
	for _, n := range secs {
		keys = append(keys, watchKey{Kind: "secrets", Namespace: pod.Namespace, Name: n})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	return keys
}

// apiSources implements podvm.Sources: watched objects come from their
// informer, everything else from the apiserver.
type apiSources struct {
	cs    kubernetes.Interface
	watch *sourceWatch

	mu     sync.Mutex
	tokens map[tokenKey]podvm.Token
}

type tokenKey struct {
	pod      types.UID
	audience string
	exp      int64
}

// forget drops the cached tokens of a pod.
func (a *apiSources) forget(uid types.UID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.tokens {
		if k.pod == uid {
			delete(a.tokens, k)
		}
	}
}

func (a *apiSources) ConfigMap(ctx context.Context, ns, name string) (*corev1.ConfigMap, error) {
	k := watchKey{Kind: "configmaps", Namespace: ns, Name: name}
	if obj, exists, ok := a.watch.cached(k); ok {
		if !exists {
			return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, name)
		}
		if cm, ok := obj.(*corev1.ConfigMap); ok {
			return cm, nil
		}
	}
	return a.cs.CoreV1().ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
}

func (a *apiSources) Secret(ctx context.Context, ns, name string) (*corev1.Secret, error) {
	k := watchKey{Kind: "secrets", Namespace: ns, Name: name}
	if obj, exists, ok := a.watch.cached(k); ok {
		if !exists {
			return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, name)
		}
		if sec, ok := obj.(*corev1.Secret); ok {
			return sec, nil
		}
	}
	return a.cs.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
}

// Token asks the apiserver for a token bound to the pod, as the kubelet
// does for a projected serviceAccountToken source.
func (a *apiSources) Token(ctx context.Context, pod *corev1.Pod, audience string, exp int64) (podvm.Token, error) {
	key := tokenKey{pod: pod.UID, audience: audience, exp: exp}
	a.mu.Lock()
	tok, ok := a.tokens[key]
	a.mu.Unlock()
	if ok && time.Now().Before(tok.RefreshAt) {
		return tok, nil
	}
	tok, err := a.newToken(ctx, pod, audience, exp)
	if err != nil {
		return tok, err
	}
	a.mu.Lock()
	if a.tokens == nil {
		a.tokens = map[tokenKey]podvm.Token{}
	}
	a.tokens[key] = tok
	a.mu.Unlock()
	return tok, nil
}

func (a *apiSources) newToken(ctx context.Context, pod *corev1.Pod, audience string, exp int64) (podvm.Token, error) {
	sa := pod.Spec.ServiceAccountName
	if sa == "" {
		sa = "default"
	}
	req := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
		ExpirationSeconds: &exp,
		BoundObjectRef: &authenticationv1.BoundObjectReference{
			APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID,
		},
	}}
	if audience != "" {
		req.Spec.Audiences = []string{audience}
	}
	issued := time.Now()
	tr, err := a.cs.CoreV1().ServiceAccounts(pod.Namespace).CreateToken(ctx, sa, req, metav1.CreateOptions{})
	if err != nil {
		return podvm.Token{}, fmt.Errorf("token for service account %s/%s: %w", pod.Namespace, sa, err)
	}
	life := tr.Status.ExpirationTimestamp.Sub(issued)
	refresh := issued.Add(time.Duration(float64(life) * tokenRefreshFraction))
	if limit := issued.Add(tokenMaxAge); refresh.After(limit) {
		refresh = limit
	}
	return podvm.Token{Value: tr.Status.Token, RefreshAt: refresh}, nil
}
