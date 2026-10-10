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

package guest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func withPath(env []string) []string {
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			return env
		}
	}
	return append(append([]string(nil), env...), "PATH="+defaultPath)
}

// lookPath finds argv[0] in the PATH of env, in the current root.
func lookPath(argv, env []string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("no command")
	}
	name := argv[0]
	if strings.Contains(name, "/") {
		return name, nil
	}
	pathEnv := defaultPath
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathEnv = v
		}
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("exec: %q: executable file not found in $PATH", name)
}

// resolveIn returns the path p names inside root, following symlinks as if
// root were /, so an image's absolute symlink cannot point a mount outside
// the container. The part that does not exist yet is joined as written.
func resolveIn(root, p string) (string, error) {
	cur := "/"
	rest := strings.Split(strings.TrimPrefix(path.Clean("/"+p), "/"), "/")
	links := 0
	for len(rest) > 0 {
		comp := rest[0]
		rest = rest[1:]
		if comp == "" || comp == "." {
			continue
		}
		if comp == ".." {
			cur = path.Dir(cur)
			continue
		}
		next := path.Join(cur, comp)
		st, err := os.Lstat(filepath.Join(root, next))
		if err != nil || st.Mode()&fs.ModeSymlink == 0 {
			cur = next
			continue
		}
		if links++; links > 40 {
			return "", fmt.Errorf("%s: too many levels of symbolic links", p)
		}
		target, err := os.Readlink(filepath.Join(root, next))
		if err != nil {
			return "", err
		}
		if path.IsAbs(target) {
			cur = "/"
		}
		rest = append(strings.Split(strings.Trim(target, "/"), "/"), rest...)
	}
	return filepath.Join(root, cur), nil
}

func hasEnv(env []string, key string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

// mergeEnv puts over on top of base, keeping the order of base.
func mergeEnv(base, over []string) []string {
	out := append([]string(nil), base...)
	for _, kv := range over {
		k, _, _ := strings.Cut(kv, "=")
		replaced := false
		for i, b := range out {
			if strings.HasPrefix(b, k+"=") {
				out[i], replaced = kv, true
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return out
}
