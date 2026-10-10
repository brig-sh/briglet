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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/brig-sh/briglet/internal/guest"
	"github.com/brig-sh/briglet/internal/hull"
)

// PodDirs are the host side of a pod's guest directory, which the VM
// mounts read-write at guest.PodDir. Volumes live inside it, so they reach
// the guest without a share of their own.
type PodDirs struct {
	// Guest is the pod's guest directory.
	Guest string
	// Volumes holds one directory per volume: <guest>/volumes/<name>.
	Volumes string
	// Subpath holds what subPath mounts bind: copies out of atomically
	// written volumes, and hostPath files.
	Subpath string
}

// NewPodDirs lays out the guest directory of a pod under dir.
func NewPodDirs(dir string) PodDirs {
	g := filepath.Join(dir, "guest")
	return PodDirs{Guest: g, Volumes: filepath.Join(g, "volumes"), Subpath: filepath.Join(g, "subpath")}
}

// VolumeDir is the host directory of a volume the agent provides.
func (d PodDirs) VolumeDir(name string) string { return filepath.Join(d.Volumes, name) }

// GuestPath maps a host path inside the guest directory to the path the
// VM sees.
func (d PodDirs) GuestPath(host string) string {
	rel, err := filepath.Rel(d.Guest, host)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	return path.Join(guest.PodDir, filepath.ToSlash(rel))
}

// HostShares collects the host directories outside the guest directory
// that a pod's VM needs: hostPath volumes. One share serves every mount
// of the same directory.
type HostShares struct {
	Shares []hull.Share
	byHost map[string]int
}

// Add shares host and returns its guest path. A share is read-only only
// while every mount of it is.
func (h *HostShares) Add(host string, ro bool) string {
	if h.byHost == nil {
		h.byHost = map[string]int{}
	}
	if i, ok := h.byHost[host]; ok {
		h.Shares[i].ReadOnly = h.Shares[i].ReadOnly && ro
		return h.Shares[i].Guest
	}
	g := fmt.Sprintf("%s/host/%d", guest.PodDir, len(h.Shares))
	h.byHost[host] = len(h.Shares)
	h.Shares = append(h.Shares, hull.Share{Host: host, Guest: g, ReadOnly: ro})
	return g
}

// PrepareVolumes creates the directories of emptyDir and hostPath volumes.
// Atomic volumes are written separately with WriteAtomic.
func PrepareVolumes(pod *corev1.Pod, dirs PodDirs) error {
	for i := range pod.Spec.Volumes {
		v := &pod.Spec.Volumes[i]
		switch {
		case v.EmptyDir != nil:
			if err := os.MkdirAll(dirs.VolumeDir(v.Name), 0o777); err != nil {
				return err
			}
			// The share stands for the guest's own directory: world
			// writable like the kubelet's emptyDir.
			if err := os.Chmod(dirs.VolumeDir(v.Name), 0o777); err != nil {
				return err
			}
		case v.HostPath != nil:
			if err := prepareHostPath(v); err != nil {
				return err
			}
		}
	}
	return nil
}

func prepareHostPath(v *corev1.Volume) error {
	p := v.HostPath.Path
	t := corev1.HostPathUnset
	if v.HostPath.Type != nil {
		t = *v.HostPath.Type
	}
	info, err := os.Stat(p)
	switch t {
	case corev1.HostPathDirectoryOrCreate:
		if errors.Is(err, fs.ErrNotExist) {
			return os.MkdirAll(p, 0o755)
		}
	case corev1.HostPathFileOrCreate:
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			return f.Close()
		}
	}
	if err != nil {
		return fmt.Errorf("hostPath %s: %w", p, err)
	}
	switch {
	case t == corev1.HostPathDirectory && !info.IsDir():
		return fmt.Errorf("hostPath %s is not a directory", p)
	case t == corev1.HostPathFile && info.IsDir():
		return fmt.Errorf("hostPath %s is not a file", p)
	}
	return nil
}

// Mounts maps the volumeMounts of container index ci to guest bind mounts.
// emptyDir and the atomically written volumes are directories inside the
// guest directory; a hostPath directory becomes a share in hs. The guest
// binds files as well as directories, so a subPath binds exactly what it
// names. A subPath into an atomically written volume binds a copy taken
// now, which later updates do not reach; the kubelet's bind mount behaves
// the same. A hostPath file is hard-linked (or copied, across file
// systems) into the guest directory.
func Mounts(pod *corev1.Pod, c *corev1.Container, ci int, dirs PodDirs, env []EnvVar, hs *HostShares) ([]guest.Mount, error) {
	vols := map[string]*corev1.Volume{}
	for i := range pod.Spec.Volumes {
		vols[pod.Spec.Volumes[i].Name] = &pod.Spec.Volumes[i]
	}
	vars := map[string]string{}
	for _, v := range env {
		vars[v.Name] = v.Value
	}
	var out []guest.Mount
	for i, m := range c.VolumeMounts {
		v := vols[m.Name]
		if v == nil {
			return nil, fmt.Errorf("mount %s: no volume %q", m.MountPath, m.Name)
		}
		mountPath := path.Clean(m.MountPath)
		atomic := Atomic(v)
		ro := m.ReadOnly || atomic
		sub := m.SubPath
		if m.SubPathExpr != "" {
			sub = expand(m.SubPathExpr, vars)
		}
		if sub != "" {
			clean := path.Clean(sub)
			if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
				return nil, fmt.Errorf("mount %s: subPath %q must stay inside the volume", mountPath, sub)
			}
			sub = clean
		}
		add := func(src string) { out = append(out, guest.Mount{Source: src, Destination: mountPath, ReadOnly: ro}) }
		// An emptyDir of Memory medium becomes a tmpfs in the container,
		// as the kubelet mounts it, not a share of the pod directory.
		if v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory && sub == "" {
			out = append(out, guest.Mount{Destination: mountPath, ReadOnly: ro, Tmpfs: true, Volume: v.Name})
			continue
		}
		copyDst := filepath.Join(dirs.Subpath, fmt.Sprintf("c%dm%d", ci, i))
		switch {
		case v.HostPath != nil:
			src := v.HostPath.Path
			if sub != "" {
				src = filepath.Join(src, sub)
			}
			info, err := os.Stat(src)
			if errors.Is(err, fs.ErrNotExist) && sub != "" {
				if err = os.MkdirAll(src, 0o755); err == nil {
					info, err = os.Stat(src)
				}
			}
			if err != nil {
				return nil, fmt.Errorf("mount %s: %w", mountPath, err)
			}
			if info.IsDir() {
				if strings.ContainsAny(src, ": \t'\"") {
					return nil, fmt.Errorf("mount %s: hull cannot share %q", mountPath, src)
				}
				add(hs.Add(src, ro))
				continue
			}
			if err := os.MkdirAll(dirs.Subpath, 0o755); err != nil {
				return nil, err
			}
			if err := linkOrCopy(src, copyDst); err != nil {
				return nil, err
			}
			add(dirs.GuestPath(copyDst))
		case atomic && sub != "":
			// Follow ..data to the current timestamp directory and copy out
			// of it, so the mount does not depend on a directory the next
			// update retires.
			real, err := filepath.EvalSymlinks(filepath.Join(dirs.VolumeDir(v.Name), sub))
			if err != nil {
				return nil, fmt.Errorf("mount %s: subPath %q: %w", mountPath, sub, err)
			}
			info, err := os.Stat(real)
			if err != nil {
				return nil, err
			}
			if err := os.MkdirAll(dirs.Subpath, 0o755); err != nil {
				return nil, err
			}
			if info.IsDir() {
				err = copyTree(real, copyDst)
			} else {
				err = copyFile(real, copyDst)
			}
			if err != nil {
				return nil, err
			}
			add(dirs.GuestPath(copyDst))
		default:
			src := dirs.VolumeDir(v.Name)
			if sub != "" {
				src = filepath.Join(src, sub)
				if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
					if err := os.MkdirAll(src, 0o777); err != nil {
						return nil, err
					}
				}
			}
			add(dirs.GuestPath(src))
		}
	}
	return out, nil
}

func linkOrCopy(src, dst string) error {
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, info.Mode().Perm())
}

// copyTree copies a directory, following symlinks inside it.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		return copyFile(p, target)
	})
}
