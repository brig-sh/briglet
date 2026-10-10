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
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/brig-sh/briglet/internal/guest"
	"github.com/brig-sh/briglet/internal/hull"
)

// BaseMemMB is what the VM gets on top of its containers' memory, for the
// guest kernel and the supervisor.
const BaseMemMB = 64

// PodResources sizes the pod's VM. CPU and memory are the sum over the
// regular and sidecar containers, or the largest init container when that
// is more, as the kubelet computes a pod's effective request. A container
// without limits counts as DefaultCPUs and DefaultMemMB. Pod-level limits
// win. Memory gets BaseMemMB on top; CPUs are capped at the host's.
func PodResources(pod *corev1.Pod) (cpus, memMB int) {
	var sumCPU, sumMem, initCPU, initMem int
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		cpu, mem := Resources(c)
		if isSidecar(c) {
			sumCPU, sumMem = sumCPU+cpu, sumMem+mem
			continue
		}
		initCPU, initMem = max(initCPU, cpu), max(initMem, mem)
	}
	for i := range pod.Spec.Containers {
		cpu, mem := Resources(&pod.Spec.Containers[i])
		sumCPU, sumMem = sumCPU+cpu, sumMem+mem
	}
	cpus, memMB = max(sumCPU, initCPU, 1), max(sumMem, initMem)
	if r := pod.Spec.Resources; r != nil {
		if q, ok := r.Limits[corev1.ResourceCPU]; ok && !q.IsZero() {
			cpus = int(math.Ceil(float64(q.MilliValue()) / 1000))
		}
		if q, ok := r.Limits[corev1.ResourceMemory]; ok && !q.IsZero() {
			memMB = int(math.Ceil(float64(q.Value()) / (1 << 20)))
		}
	}
	return min(cpus, runtime.NumCPU()), memMB + BaseMemMB
}

func isSidecar(c *corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// Hostname is the pod's hostname: spec.hostname or the pod name, cut to 63
// characters as the kubelet does.
func Hostname(pod *corev1.Pod) string {
	name := pod.Spec.Hostname
	if name == "" {
		name = pod.Name
	}
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-.")
	}
	return name
}

// HostsFile is the pod's /etc/hosts, in the kubelet's layout: the
// standard entries, the pod's address and name (with its subdomain FQDN
// when spec.subdomain is set), then spec.hostAliases.
func HostsFile(pod *corev1.Pod, ip, domain string) string {
	var b strings.Builder
	b.WriteString("# Kubernetes-managed hosts file.\n")
	b.WriteString("127.0.0.1\tlocalhost\n")
	b.WriteString("::1\tlocalhost ip6-localhost ip6-loopback\n")
	b.WriteString("fe00::0\tip6-localnet\n")
	b.WriteString("fe00::0\tip6-mcastprefix\n")
	b.WriteString("fe00::1\tip6-allnodes\n")
	b.WriteString("fe00::2\tip6-allrouters\n")
	name := Hostname(pod)
	if pod.Spec.Subdomain != "" {
		fmt.Fprintf(&b, "%s\t%s.%s.%s.svc.%s\t%s\n", ip, name, pod.Spec.Subdomain, pod.Namespace, domain, name)
	} else {
		fmt.Fprintf(&b, "%s\t%s\n", ip, name)
	}
	if len(pod.Spec.HostAliases) > 0 {
		b.WriteString("\n# Entries added by HostAliases.\n")
		for _, a := range pod.Spec.HostAliases {
			fmt.Fprintf(&b, "%s\t%s\n", a.IP, strings.Join(a.Hostnames, "\t"))
		}
	}
	return b.String()
}

// User is who a container runs as.
type User struct {
	UID, GID uint32
	Groups   []uint32
	Home     string
}

type passwdEntry struct {
	name     string
	uid, gid uint32
	home     string
}

// ResolveUser computes the container's user from the image USER, the
// image's /etc/passwd and /etc/group under rootfs, and the pod and
// container securityContext: runAsUser and runAsGroup override the image,
// supplementalGroups and fsGroup join the groups the image lists for the
// user, and runAsNonRoot refuses uid 0.
func ResolveUser(pod *corev1.Pod, c *corev1.Container, img *hull.ImageConfig, rootfs string) (User, error) {
	passwd := readPasswd(filepath.Join(rootfs, "etc", "passwd"))
	groups := readGroups(filepath.Join(rootfs, "etc", "group"))
	byUID := func(uid uint32) *passwdEntry {
		for i := range passwd {
			if passwd[i].uid == uid {
				return &passwd[i]
			}
		}
		return nil
	}
	spec := img.User
	if spec == "" {
		spec = "0"
	}
	userPart, groupPart, hasGroup := strings.Cut(spec, ":")
	var u User
	var entry *passwdEntry
	if n, err := strconv.ParseUint(userPart, 10, 32); err == nil {
		u.UID = uint32(n)
		entry = byUID(u.UID)
	} else {
		for i := range passwd {
			if passwd[i].name == userPart {
				entry = &passwd[i]
			}
		}
		if entry == nil {
			return u, fmt.Errorf("unable to find user %s: no matching entries in passwd file", userPart)
		}
		u.UID = entry.uid
	}
	if entry != nil {
		u.GID = entry.gid
	}
	if hasGroup {
		if n, err := strconv.ParseUint(groupPart, 10, 32); err == nil {
			u.GID = uint32(n)
		} else if g, ok := groups[groupPart]; ok {
			u.GID = g.gid
		} else {
			return u, fmt.Errorf("unable to find group %s: no matching entries in group file", groupPart)
		}
	}
	runAsUser, runAsGroup, nonRoot := securityIDs(pod, c)
	if runAsUser != nil {
		u.UID = uint32(*runAsUser)
		entry = byUID(u.UID)
		u.GID = 0
		if entry != nil {
			u.GID = entry.gid
		}
	}
	if runAsGroup != nil {
		u.GID = uint32(*runAsGroup)
	}
	if nonRoot && u.UID == 0 {
		return u, fmt.Errorf("container has runAsNonRoot and image will run as root (pod: %q, container: %s)",
			pod.Name+"_"+pod.Namespace+"("+string(pod.UID)+")", c.Name)
	}
	seen := map[uint32]bool{}
	addGroup := func(g uint32) {
		if !seen[g] {
			seen[g] = true
			u.Groups = append(u.Groups, g)
		}
	}
	if entry != nil {
		var names []string
		for name := range groups {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			for _, m := range groups[name].members {
				if m == entry.name {
					addGroup(groups[name].gid)
				}
			}
		}
	}
	if sc := pod.Spec.SecurityContext; sc != nil {
		for _, g := range sc.SupplementalGroups {
			addGroup(uint32(g))
		}
		if sc.FSGroup != nil {
			addGroup(uint32(*sc.FSGroup))
		}
	}
	u.Home = "/"
	if entry != nil && entry.home != "" {
		u.Home = entry.home
	}
	return u, nil
}

func securityIDs(pod *corev1.Pod, c *corev1.Container) (user, group *int64, nonRoot bool) {
	if sc := pod.Spec.SecurityContext; sc != nil {
		user, group = sc.RunAsUser, sc.RunAsGroup
		nonRoot = sc.RunAsNonRoot != nil && *sc.RunAsNonRoot
	}
	if sc := c.SecurityContext; sc != nil {
		if sc.RunAsUser != nil {
			user = sc.RunAsUser
		}
		if sc.RunAsGroup != nil {
			group = sc.RunAsGroup
		}
		if sc.RunAsNonRoot != nil {
			nonRoot = *sc.RunAsNonRoot
		}
	}
	return user, group, nonRoot
}

func readPasswd(p string) []passwdEntry {
	var out []passwdEntry
	eachLine(p, func(f []string) {
		if len(f) < 4 {
			return
		}
		uid, err1 := strconv.ParseUint(f[2], 10, 32)
		gid, err2 := strconv.ParseUint(f[3], 10, 32)
		if err1 != nil || err2 != nil {
			return
		}
		e := passwdEntry{name: f[0], uid: uint32(uid), gid: uint32(gid)}
		if len(f) > 5 {
			e.home = f[5]
		}
		out = append(out, e)
	})
	return out
}

type groupEntry struct {
	gid     uint32
	members []string
}

func readGroups(p string) map[string]groupEntry {
	out := map[string]groupEntry{}
	eachLine(p, func(f []string) {
		if len(f) < 3 {
			return
		}
		gid, err := strconv.ParseUint(f[2], 10, 32)
		if err != nil {
			return
		}
		g := groupEntry{gid: uint32(gid)}
		if len(f) > 3 && f[3] != "" {
			g.members = strings.Split(f[3], ",")
		}
		out[f[0]] = g
	})
	return out
}

// eachLine calls fn with the colon-separated fields of each line of a
// passwd-style file. A missing file has no lines.
func eachLine(p string, fn func([]string)) {
	f, err := os.Open(p)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fn(strings.Split(line, ":"))
	}
}

// ContainerInput is what one container entry of pod.json is built from.
type ContainerInput struct {
	Container *corev1.Container
	Init      bool
	// Index is the container's position in pod.json, which names its
	// subPath copies.
	Index  int
	Image  *hull.ImageConfig
	Rootfs string // the image rootfs on the host, for /etc/passwd
	// GuestRootfs is where the VM sees the image rootfs.
	GuestRootfs string
	Env         []EnvVar
}

// Etc files every container binds from the guest directory, and where a
// container's termination message file lives.
const (
	GuestHosts      = guest.PodDir + "/etc/hosts"
	GuestHostname   = guest.PodDir + "/etc/hostname"
	GuestResolvConf = guest.PodDir + "/etc/resolv.conf"
	guestTermDir    = guest.PodDir + "/term"
)

// TermFile is the host path of a container's termination message file.
func TermFile(dirs PodDirs, name string) string { return filepath.Join(dirs.Guest, "term", name) }

// Container builds one pod.json entry: argv, env, user, working
// directory, mounts and the limits the supervisor applies.
func Container(pod *corev1.Pod, in ContainerInput, dirs PodDirs, hs *HostShares) (guest.Container, error) {
	c := in.Container
	pairs := Pairs(in.Env)
	argv, err := Argv(c, in.Image, pairs)
	if err != nil {
		return guest.Container{}, err
	}
	u, err := ResolveUser(pod, c, in.Image, in.Rootfs)
	if err != nil {
		return guest.Container{}, err
	}
	if !hasEnv(pairs, "HOME") {
		pairs = append(pairs, "HOME="+u.Home)
	}
	mounts, err := Mounts(pod, c, in.Index, dirs, in.Env, hs)
	if err != nil {
		return guest.Container{}, err
	}
	termPath := c.TerminationMessagePath
	if termPath == "" {
		termPath = corev1.TerminationMessagePathDefault
	}
	mounted := map[string]bool{}
	for _, m := range mounts {
		mounted[m.Destination] = true
	}
	for _, sys := range []guest.Mount{
		{Source: GuestHosts, Destination: "/etc/hosts"},
		{Source: GuestHostname, Destination: "/etc/hostname"},
		{Source: GuestResolvConf, Destination: "/etc/resolv.conf"},
		{Source: guestTermDir + "/" + c.Name, Destination: termPath},
	} {
		if !mounted[sys.Destination] {
			mounts = append(mounts, sys)
		}
	}
	// Parents first, as containerd orders them: a volume at /etc would
	// otherwise hide /etc/hosts.
	sort.SliceStable(mounts, func(i, j int) bool {
		return strings.Count(mounts[i].Destination, "/") < strings.Count(mounts[j].Destination, "/")
	})
	cwd := c.WorkingDir
	if cwd == "" {
		cwd = in.Image.WorkingDir
	}
	if cwd == "" {
		cwd = "/"
	}
	_, mem := Resources(c)
	out := guest.Container{
		Name:           c.Name,
		Init:           in.Init,
		RestartAlways:  in.Init && isSidecar(c),
		Rootfs:         in.GuestRootfs,
		Argv:           argv,
		Env:            pairs,
		Cwd:            cwd,
		UID:            u.UID,
		GID:            u.GID,
		Groups:         u.Groups,
		MemoryMB:       mem,
		Mounts:         mounts,
		TerminationLog: guestTermDir + "/" + c.Name,
		FallbackToLogs: c.TerminationMessagePolicy == corev1.TerminationMessageFallbackToLogsOnError,
	}
	if sc := c.SecurityContext; sc != nil {
		out.ReadOnlyRootfs = sc.ReadOnlyRootFilesystem != nil && *sc.ReadOnlyRootFilesystem
		out.Privileged = sc.Privileged != nil && *sc.Privileged
		out.NoNewPrivs = sc.AllowPrivilegeEscalation != nil && !*sc.AllowPrivilegeEscalation
	}
	if lc := c.Lifecycle; lc != nil {
		out.Lifecycle = &guest.Lifecycle{PostStart: handler(c, lc.PostStart), PreStop: handler(c, lc.PreStop)}
	}
	return out, nil
}

// handler maps one lifecycle handler of the pod spec. The HTTP GET keeps
// only what the supervisor needs: the resolved port and the host, which
// defaults to the pod's own address inside the VM.
func handler(c *corev1.Container, h *corev1.LifecycleHandler) *guest.LifecycleHandler {
	if h == nil {
		return nil
	}
	out := &guest.LifecycleHandler{}
	if e := h.Exec; e != nil {
		out.Exec = append([]string{}, e.Command...)
	}
	if g := h.HTTPGet; g != nil {
		host := g.Host
		if host == "" {
			host = "127.0.0.1"
		}
		scheme := strings.ToLower(string(g.Scheme))
		if scheme == "" {
			scheme = "http"
		}
		out.HTTP = &guest.HTTPGet{Scheme: scheme, Host: host, Port: httpGetPort(c, g.Port), Path: g.Path}
	}
	if out.Exec == nil && out.HTTP == nil {
		return nil
	}
	return out
}

// httpGetPort resolves an int-or-name port against the container's
// ports, as the kubelet does for handlers.
func httpGetPort(c *corev1.Container, p intstr.IntOrString) int {
	if p.Type == intstr.Int {
		return p.IntValue()
	}
	for i := range c.Ports {
		if c.Ports[i].Name == p.StrVal {
			return int(c.Ports[i].ContainerPort)
		}
	}
	return 0
}

func hasEnv(pairs []string, key string) bool {
	for _, kv := range pairs {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

// Sysctls are the pod's securityContext sysctls.
func Sysctls(pod *corev1.Pod) map[string]string {
	sc := pod.Spec.SecurityContext
	if sc == nil || len(sc.Sysctls) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, s := range sc.Sysctls {
		out[s.Name] = s.Value
	}
	return out
}
