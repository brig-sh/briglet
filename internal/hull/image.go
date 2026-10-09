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

package hull

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Image is a pulled image as the store records it.
type Image struct {
	Ref         string `json:"ref"`
	Digest      string `json:"digest"`
	IndexDigest string `json:"indexDigest"`
	Platform    string `json:"platform"`
}

// ImageConfig is the part of the OCI image config a container start needs.
type ImageConfig struct {
	Entrypoint []string `json:"Entrypoint"`
	Cmd        []string `json:"Cmd"`
	Env        []string `json:"Env"`
	WorkingDir string   `json:"WorkingDir"`
	User       string   `json:"User"`
}

// ID is the image identity reported in container status: the index digest
// when the reference resolved through one, the manifest digest otherwise.
func (i *Image) ID() string {
	if i.IndexDigest != "" {
		return i.IndexDigest
	}
	return i.Digest
}

// LocalImage finds an image in the store pulled under exactly ref. The store
// keeps images/<digest>/image.json; see pkg/store in hull.
func (c *Client) LocalImage(ref string) (*Image, bool) {
	matches, _ := filepath.Glob(filepath.Join(c.StoreDir, "images", "*", "image.json"))
	var found *Image
	var newest os.FileInfo
	for _, m := range matches {
		img, err := readImage(m)
		if err != nil || img.Ref != ref {
			continue
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(m), "oci-config.json")); err != nil {
			continue
		}
		st, err := os.Stat(m)
		if err != nil {
			continue
		}
		if newest == nil || st.ModTime().After(newest.ModTime()) {
			found, newest = img, st
		}
	}
	return found, found != nil
}

// ImageByDigest reads the record of a pulled image.
func (c *Client) ImageByDigest(digest string) (*Image, error) {
	return readImage(filepath.Join(c.StoreDir, "images", digest, "image.json"))
}

func readImage(path string) (*Image, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var img Image
	if err := json.Unmarshal(b, &img); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &img, nil
}

// Config reads the OCI image config hull saved next to the unpacked rootfs.
func (c *Client) Config(digest string) (*ImageConfig, error) {
	path := filepath.Join(c.StoreDir, "images", digest, "oci-config.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Config ImageConfig `json:"config"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &doc.Config, nil
}
