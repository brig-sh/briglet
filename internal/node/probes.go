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
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/brig-sh/briglet/internal/guest"
	"github.com/brig-sh/briglet/internal/hull"
	"github.com/brig-sh/briglet/internal/podvm"
)

const (
	probeStartup   = "startup"
	probeLiveness  = "liveness"
	probeReadiness = "readiness"
	// maxProbeOutput bounds what a probe message keeps, as the kubelet does.
	maxProbeOutput = 10 << 10
	probeUserAgent = "kube-probe/1.37"
)

type probeSpec struct {
	kind string
	p    *corev1.Probe
}

// probesOf lists the container's probes, the startup probe first.
func probesOf(c *corev1.Container) []probeSpec {
	var out []probeSpec
	if c.StartupProbe != nil {
		out = append(out, probeSpec{probeStartup, c.StartupProbe})
	}
	if c.LivenessProbe != nil {
		out = append(out, probeSpec{probeLiveness, c.LivenessProbe})
	}
	if c.ReadinessProbe != nil {
		out = append(out, probeSpec{probeReadiness, c.ReadinessProbe})
	}
	return out
}

func seconds(v, def int32) time.Duration {
	if v <= 0 {
		v = def
	}
	return time.Duration(v) * time.Second
}

func threshold(v, def int32) int32 {
	if v <= 0 {
		return def
	}
	return v
}

// probeDue is when the probe runs next: initialDelaySeconds after the
// container started, then every periodSeconds.
func probeDue(ps probeSpec, cst *ctrState) time.Time {
	first := cst.Run.Add(time.Duration(ps.p.InitialDelaySeconds) * time.Second)
	last := cst.Probes[ps.kind]
	if last == nil || last.LastAt.Before(cst.Run) {
		return first
	}
	next := last.LastAt.Add(seconds(ps.p.PeriodSeconds, 10))
	if next.Before(first) {
		return first
	}
	return next
}

// initialProbeState is the startup and readiness state of a container that
// just started: started unless a startup probe gates it, ready only when
// started and no readiness probe decides.
func initialProbeState(c *corev1.Container, cst *ctrState) {
	cst.Probes = nil
	cst.Started = c.StartupProbe == nil
	cst.Ready = cst.Started && c.ReadinessProbe == nil
}

// probe runs the probes that are due in every running container. A startup
// or liveness probe at its failure threshold restarts that container
// through the supervisor. It reports whether a startup or readiness state
// changed, and when the next probe is due.
func (w *podWorker) probe(ctx context.Context, pod *corev1.Pod, now time.Time) (changed bool, next time.Time) {
	for _, c := range podvm.AllContainers(pod) {
		probes := probesOf(c)
		if len(probes) == 0 {
			continue
		}
		st := w.snapshot()
		cs := st.Guest.Find(c.Name)
		cst := st.Containers[c.Name]
		if cs == nil || cs.State != guest.StateRunning || cst == nil || !cst.Run.Equal(cs.StartedAt) {
			continue
		}
		ch, n, kill := w.probeContainer(ctx, c, probes, now)
		changed = changed || ch
		next = earliest(next, n)
		if kill != "" {
			w.restartContainer(ctx, pod, c, kill)
		}
		if ctx.Err() != nil || w.stopping() {
			return changed, next
		}
	}
	return changed, next
}

func (w *podWorker) probeContainer(ctx context.Context, c *corev1.Container, probes []probeSpec, now time.Time) (changed bool, next time.Time, kill string) {
	for _, ps := range probes {
		st := w.snapshot()
		cst := st.Containers[c.Name]
		if cst == nil {
			return changed, next, ""
		}
		if (ps.kind == probeStartup) == cst.Started {
			// The startup probe runs until it passes; the others after.
			continue
		}
		due := probeDue(ps, cst)
		if now.Before(due) {
			next = earliest(next, due)
			continue
		}
		ok, msg := w.runProbe(ctx, c, &st, ps.p)
		if ctx.Err() != nil || w.stopping() {
			return changed, next, ""
		}
		var failures int32
		w.set(func(st *podState) {
			cst := st.ctr(c.Name)
			if cst.Probes == nil {
				cst.Probes = map[string]*probeState{}
			}
			r := cst.Probes[ps.kind]
			if r == nil {
				r = &probeState{}
				cst.Probes[ps.kind] = r
			}
			r.LastAt = time.Now()
			if ok {
				r.Successes++
				r.Failures = 0
				r.Result, r.Message = "success", ""
			} else {
				r.Failures++
				r.Successes = 0
				r.Result, r.Message = "failure", msg
			}
			failures = r.Failures
			wasStarted, wasReady := cst.Started, cst.Ready
			switch ps.kind {
			case probeStartup:
				if ok {
					cst.Started = true
					cst.Ready = c.ReadinessProbe == nil
				}
			case probeReadiness:
				if ok && r.Successes >= threshold(ps.p.SuccessThreshold, 1) {
					cst.Ready = true
				}
				if !ok && r.Failures >= threshold(ps.p.FailureThreshold, 3) {
					cst.Ready = false
				}
			}
			changed = changed || cst.Started != wasStarted || cst.Ready != wasReady
		})
		next = earliest(next, time.Now().Add(seconds(ps.p.PeriodSeconds, 10)))
		if ok {
			continue
		}
		kind := strings.ToUpper(ps.kind[:1]) + ps.kind[1:]
		w.event(corev1.EventTypeWarning, "Unhealthy", "%s probe failed: %s", kind, msg)
		w.logf("%s probe of %s failed (%d in a row): %s", ps.kind, c.Name, failures, msg)
		if ps.kind != probeReadiness && failures >= threshold(ps.p.FailureThreshold, 3) {
			w.event(corev1.EventTypeNormal, "Killing", "Container %s failed %s probe, will be restarted", c.Name, ps.kind)
			return changed, next, ps.kind
		}
	}
	return changed, next, ""
}

// restartContainer has the supervisor stop a container whose startup or
// liveness probe failed, with the probe's terminationGracePeriodSeconds or
// the pod's. The restart policy decides what follows, in the guest.
func (w *podWorker) restartContainer(ctx context.Context, pod *corev1.Pod, c *corev1.Container, kind string) {
	grace := podvm.GraceSeconds(pod)
	for _, ps := range probesOf(c) {
		if ps.kind == kind && ps.p.TerminationGracePeriodSeconds != nil {
			grace = int(*ps.p.TerminationGracePeriodSeconds)
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := w.ctl(cctx, guest.CtlRequest{Op: guest.CtlRestart, Container: c.Name, Grace: grace}); err != nil {
		w.logf("restart %s after the %s probe failed: %v", c.Name, kind, err)
		return
	}
	// Probes wait for the next run.
	w.set(func(st *podState) { st.ctr(c.Name).Run = time.Time{} })
	w.logf("asked the supervisor to restart %s after the %s probe failed (grace %d s)", c.Name, kind, grace)
}

func earliest(a, b time.Time) time.Time {
	if b.IsZero() {
		return a
	}
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

// runProbe runs one probe attempt within its timeoutSeconds.
func (w *podWorker) runProbe(ctx context.Context, c *corev1.Container, st *podState, p *corev1.Probe) (bool, string) {
	timeout := seconds(p.TimeoutSeconds, 1)
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch {
	case p.Exec != nil:
		return w.execProbe(pctx, c.Name, st, p.Exec.Command, timeout)
	case p.HTTPGet != nil:
		return w.httpProbe(pctx, c, st, p.HTTPGet)
	case p.TCPSocket != nil:
		return w.tcpProbe(pctx, c, st, p.TCPSocket.Port)
	case p.GRPC != nil:
		return w.grpcProbe(pctx, st, p.GRPC)
	}
	return false, "probe has no handler"
}

// execProbe runs the command in the container through the guest agent and
// briglet-guest exec, with the container's env. Exit 0 is success.
func (w *podWorker) execProbe(ctx context.Context, container string, st *podState, argv []string, timeout time.Duration) (bool, string) {
	sess, err := hull.DialAgent(ctx, w.m.hull.AgentSocket(st.Instance), hull.OpenRequest{Argv: guestExec(container, argv)})
	if err != nil {
		return false, err.Error()
	}
	defer sess.Close()
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()
	_ = sess.CopyStdin(strings.NewReader(""))
	out := &limitedBuffer{max: maxProbeOutput}
	code, err := sess.Wait(out, out)
	msg := strings.TrimSpace(out.String())
	switch {
	case ctx.Err() != nil:
		return false, fmt.Sprintf("command %q timed out after %s", argv, timeout)
	case err != nil:
		return false, err.Error()
	case code != 0:
		if msg == "" {
			msg = fmt.Sprintf("command %q exited with %d", argv, code)
		}
		return false, msg
	}
	return true, msg
}

// httpProbe sends the request through the gateway forward of the port, with
// Host set to the pod address as the kubelet's request would carry it.
// 200-399 is success; redirects are not followed.
func (w *podWorker) httpProbe(ctx context.Context, c *corev1.Container, st *podState, h *corev1.HTTPGetAction) (bool, string) {
	port, err := resolvePort(h.Port, c)
	if err != nil {
		return false, err.Error()
	}
	local := st.Forwards[strconv.Itoa(int(port))]
	if local == "" {
		if local, err = w.ensureForward(ctx, port); err != nil {
			return false, err.Error()
		}
	}
	scheme := "http"
	if h.Scheme == corev1.URISchemeHTTPS {
		scheme = "https"
	}
	p := h.Path
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u, err := url.Parse(p)
	if err != nil {
		return false, err.Error()
	}
	u.Scheme, u.Host = scheme, local
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, err.Error()
	}
	req.Host = net.JoinHostPort(st.IP, strconv.Itoa(int(port)))
	req.Header.Set("User-Agent", probeUserAgent)
	req.Header.Set("Accept", "*/*")
	for _, hh := range h.HTTPHeaders {
		if strings.EqualFold(hh.Name, "Host") {
			req.Host = hh.Value
			continue
		}
		req.Header.Add(hh.Name, hh.Value)
	}
	resp, err := w.m.probeClient.Do(req)
	if err != nil {
		return false, strings.ReplaceAll(err.Error(), u.Host, req.Host)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxProbeOutput))
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return true, ""
	}
	return false, fmt.Sprintf("HTTP probe failed with statuscode: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// tcpProbe asks the gateway to dial the pod port inside the virtual network.
func (w *podWorker) tcpProbe(ctx context.Context, c *corev1.Container, st *podState, p intstr.IntOrString) (bool, string) {
	port, err := resolvePort(p, c)
	if err != nil {
		return false, err.Error()
	}
	addr := net.JoinHostPort(st.IP, strconv.Itoa(int(port)))
	ok, err := w.m.gw.ProbeTCP(ctx, addr)
	switch {
	case err != nil:
		return false, err.Error()
	case !ok:
		return false, fmt.Sprintf("dial tcp %s: connection refused", addr)
	}
	return true, ""
}

// resolvePort turns a probe port, a number or a container port name, into a
// number.
func resolvePort(p intstr.IntOrString, c *corev1.Container) (int32, error) {
	if p.Type == intstr.Int {
		return p.IntVal, nil
	}
	if n, err := strconv.Atoi(p.StrVal); err == nil {
		return int32(n), nil
	}
	for _, cp := range c.Ports {
		if cp.Name == p.StrVal {
			return cp.ContainerPort, nil
		}
	}
	return 0, fmt.Errorf("port %q is not a port of container %s", p.StrVal, c.Name)
}

// probePorts lists the ports that get a gateway forward: every TCP
// container port and every httpGet and gRPC probe port.
func probePorts(c *corev1.Container) []int32 {
	seen := map[int32]bool{}
	var out []int32
	add := func(p int32) {
		if p > 0 && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, cp := range c.Ports {
		if cp.Protocol == "" || cp.Protocol == corev1.ProtocolTCP {
			add(cp.ContainerPort)
		}
	}
	for _, ps := range probesOf(c) {
		if ps.p.HTTPGet != nil {
			if p, err := resolvePort(ps.p.HTTPGet.Port, c); err == nil {
				add(p)
			}
		}
		if ps.p.GRPC != nil {
			add(ps.p.GRPC.Port)
		}
	}
	return out
}

// ensureForwards makes the gateway forward an ephemeral 127.0.0.1 port to
// each port of the pod's containers, keeping the local ports the state
// already names.
func (w *podWorker) ensureForwards(ctx context.Context, pod *corev1.Pod) {
	var ports []int32
	seen := map[int32]bool{}
	for _, c := range podvm.AllContainers(pod) {
		for _, p := range probePorts(c) {
			if !seen[p] {
				seen[p] = true
				ports = append(ports, p)
			}
		}
	}
	if len(ports) == 0 {
		return
	}
	have, err := w.m.gw.Forwards(ctx)
	if err != nil {
		w.logf("list forwards: %v", err)
		return
	}
	live := map[hull.Forward]bool{}
	for _, f := range have {
		live[f] = true
	}
	st := w.snapshot()
	for _, p := range ports {
		remote := net.JoinHostPort(st.IP, strconv.Itoa(int(p)))
		if local := st.Forwards[strconv.Itoa(int(p))]; local != "" && live[hull.Forward{Protocol: "tcp", Local: local, Remote: remote}] {
			continue
		}
		if _, err := w.ensureForward(ctx, p); err != nil {
			w.logf("forward port %d: %v", p, err)
		}
	}
}

// ensureForward (re)creates the forward of one port: on the local port the
// state names if it is still free, on a new ephemeral one otherwise.
func (w *podWorker) ensureForward(ctx context.Context, port int32) (string, error) {
	st := w.snapshot()
	key := strconv.Itoa(int(port))
	remote := net.JoinHostPort(st.IP, key)
	try := func(local string) error {
		_ = w.m.gw.RemoveForward(ctx, "tcp", local)
		return w.m.gw.AddForward(ctx, hull.Forward{Protocol: "tcp", Local: local, Remote: remote})
	}
	local := st.Forwards[key]
	if local == "" || try(local) != nil {
		var err error
		for range 3 {
			if local, err = freeLocalPort(); err != nil {
				continue
			}
			if err = try(local); err == nil {
				break
			}
		}
		if err != nil {
			return "", err
		}
	}
	w.set(func(st *podState) {
		if st.Forwards == nil {
			st.Forwards = map[string]string{}
		}
		st.Forwards[key] = local
	})
	w.logf("gateway forwards %s to %s", local, remote)
	return local, nil
}

// freeLocalPort picks an ephemeral port on 127.0.0.1. The gateway binds it
// right after, so another process can take it in between; the caller
// retries.
func freeLocalPort() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	return addr, ln.Close()
}

// removeForwards withdraws the forwards a pod state names.
func removeForwards(ctx context.Context, gw *hull.Gateway, st *podState) {
	for _, local := range st.Forwards {
		if err := gw.RemoveForward(ctx, "tcp", local); err != nil {
			log.Printf("pod %s/%s: remove forward %s: %v", st.Namespace, st.Name, local, err)
		}
	}
}

func newProbeClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // as the kubelet's prober
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// limitedBuffer keeps the first max bytes written to it.
type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}
