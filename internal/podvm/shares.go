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
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/brig-sh/briglet/internal/hull"
)

// PodDirs are the host directories of one pod's volumes.
type PodDirs struct {
	// Volumes holds one directory per volume: <pod dir>/volumes/<name>.
	Volumes string
	// Subpath holds what subPath mounts share: copies out of atomically
	// written volumes, and directories that carry a single file.
	Subpath string
}

// VolumeDir is the host directory of a volume the agent provides.
func (d PodDirs) VolumeDir(name string) string { return filepath.Join(d.Volumes, name) }

// guardedParents are guest directories a file subPath mount must not cover:
// a file subPath is shared as its parent directory, which hides the image's
// files there.
var guardedParents = map[string]bool{
	"/": true, "/bin": true, "/sbin": true, "/lib": true, "/lib64": true, "/etc": true,
	"/usr": true, "/usr/bin": true, "/usr/sbin": true, "/usr/lib": true, "/usr/local": true,
	"/usr/local/bin": true, "/var": true, "/dev": true, "/proc": true, "/sys": true, "/run": true,
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

// Shares maps the container's volumeMounts to hull shares. A mount without
// subPath shares the volume's directory. A directory subPath shares that
// directory, created when missing as the kubelet does. A file subPath, and a
// hostPath that names a file, shares a directory that holds only that file
// (a hard link, or a copy across file systems) at the mount's parent
// directory, so the guest sees no other file of the image in that parent.
// A subPath into an atomically written volume is a copy taken now, which
// later updates do not reach; the kubelet's bind mount behaves the same.
func Shares(pod *corev1.Pod, c *corev1.Container, dirs PodDirs, env []EnvVar) ([]hull.Share, error) {
	vols := map[string]*corev1.Volume{}
	for i := range pod.Spec.Volumes {
		vols[pod.Spec.Volumes[i].Name] = &pod.Spec.Volumes[i]
	}
	vars := map[string]string{}
	for _, v := range env {
		vars[v.Name] = v.Value
	}
	if err := os.RemoveAll(dirs.Subpath); err != nil {
		return nil, err
	}
	var shares []hull.Share
	type fileGroup struct {
		host string
		ro   bool
	}
	groups := map[string]*fileGroup{}
	var groupOrder []string
	addFile := func(src, mountPath string, ro bool) error {
		parent := path.Dir(mountPath)
		if guardedParents[parent] {
			return fmt.Errorf("mount %s: briglet shares a single file as its parent directory, and %s cannot be covered", mountPath, parent)
		}
		g := groups[parent]
		if g == nil {
			g = &fileGroup{host: filepath.Join(dirs.Subpath, "f"+strconv.Itoa(len(groups))), ro: true}
			groups[parent] = g
			groupOrder = append(groupOrder, parent)
			if err := os.MkdirAll(g.host, 0o755); err != nil {
				return err
			}
		}
		g.ro = g.ro && ro
		return linkOrCopy(src, filepath.Join(g.host, path.Base(mountPath)))
	}
	for i, m := range c.VolumeMounts {
		v := vols[m.Name]
		if v == nil {
			return nil, fmt.Errorf("mount %s: no volume %q", m.MountPath, m.Name)
		}
		mountPath := path.Clean(m.MountPath)
		atomic := Atomic(v)
		ro := m.ReadOnly || atomic
		base := dirs.VolumeDir(v.Name)
		if v.HostPath != nil {
			base = v.HostPath.Path
		}
		sub := m.SubPath
		if m.SubPathExpr != "" {
			sub = expand(m.SubPathExpr, vars)
		}
		if sub != "" {
			clean := path.Clean(sub)
			if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
				return nil, fmt.Errorf("mount %s: subPath %q must stay inside the volume", mountPath, sub)
			}
		}
		src := base
		if sub != "" {
			src = filepath.Join(base, sub)
		}
		if atomic && sub != "" {
			// Follow ..data to the current timestamp directory and copy out
			// of it, so the share does not depend on a directory the next
			// update retires.
			real, err := filepath.EvalSymlinks(src)
			if err != nil {
				return nil, fmt.Errorf("mount %s: subPath %q: %w", mountPath, sub, err)
			}
			info, err := os.Stat(real)
			if err != nil {
				return nil, err
			}
			if !info.IsDir() {
				if err := addFile(real, mountPath, ro); err != nil {
					return nil, err
				}
				continue
			}
			dst := filepath.Join(dirs.Subpath, "d"+strconv.Itoa(i))
			if err := copyTree(real, dst); err != nil {
				return nil, err
			}
			shares = append(shares, hull.Share{Host: dst, Guest: mountPath, ReadOnly: ro})
			continue
		}
		info, err := os.Stat(src)
		if errors.Is(err, fs.ErrNotExist) && sub != "" {
			if err := os.MkdirAll(src, 0o755); err != nil {
				return nil, err
			}
			info, err = os.Stat(src)
		}
		if err != nil {
			return nil, fmt.Errorf("mount %s: %w", mountPath, err)
		}
		if !info.IsDir() {
			if err := addFile(src, mountPath, ro); err != nil {
				return nil, err
			}
			continue
		}
		shares = append(shares, hull.Share{Host: src, Guest: mountPath, ReadOnly: ro})
	}
	for _, parent := range groupOrder {
		g := groups[parent]
		shares = append(shares, hull.Share{Host: g.host, Guest: parent, ReadOnly: g.ro})
	}
	return shares, nil
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
