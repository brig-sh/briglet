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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/brig-sh/briglet/internal/guest"
)

// podState is what the agent knows about one pod beyond the API object. It
// is persisted in <state>/pods/<uid>/state.json so a restarted agent adopts
// the running VM instead of booting another.
type podState struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Instance  string `json:"instance"`
	IP        string `json:"ip,omitempty"`
	// Images maps a container name to the image ID it runs.
	Images map[string]string `json:"images,omitempty"`

	// Created is set once a VM for the pod has existed. Running is set
	// while the VM runs; StartedAt is when it booted.
	Created   bool      `json:"created,omitempty"`
	Running   bool      `json:"running"`
	StartedAt time.Time `json:"startedAt,omitzero"`
	// Done is set when the pod reached Succeeded or Failed and will not run
	// again. Phase is that phase; Reason and Message explain an end the
	// guest did not report itself.
	Done    bool   `json:"done,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Rejected holds why the pod cannot run on briglet.
	Rejected string `json:"rejected,omitempty"`
	// RejectedReason is the status reason of a rejection; empty means
	// podvm.ReasonUnsupported.
	RejectedReason string `json:"rejectedReason,omitempty"`
	// Deadline is set when activeDeadlineSeconds elapsed and the pod was
	// stopped for it.
	Deadline bool `json:"deadline,omitempty"`

	// Waiting is why the containers wait while no VM runs: a pull, a
	// config error, a failed boot.
	Waiting *waitingState `json:"waiting,omitempty"`
	// Guest is the last status.json of the current or the last VM.
	Guest *guest.Status `json:"guest,omitempty"`
	// Restarts carries the containers' restart counts into the next VM.
	Restarts map[string]int32 `json:"restarts,omitempty"`
	// Containers holds the probe results of each container's current run.
	Containers map[string]*ctrState `json:"containers,omitempty"`
	// Forwards maps a container port to the 127.0.0.1 address the gateway
	// forwards to it. httpGet probes use them.
	Forwards map[string]string `json:"forwards,omitempty"`
	// TokenRefreshAt is when the next service account token in a volume is
	// due. Zero when no volume holds one.
	TokenRefreshAt time.Time `json:"tokenRefreshAt,omitzero"`
}

// ctrState is the agent's side of one container run: probe results and
// what it already announced with events.
type ctrState struct {
	// Run is the start time of the run the fields describe.
	Run time.Time `json:"run"`
	// Started is set once the startup probe passed, or at start without
	// one. Ready is the readiness the probes give the container.
	Started bool                   `json:"started,omitempty"`
	Ready   bool                   `json:"ready,omitempty"`
	Probes  map[string]*probeState `json:"probes,omitempty"`
	// BackOff is the end of the run the last BackOff event was about.
	BackOff time.Time `json:"backOff,omitzero"`
}

type probeState struct {
	Successes int32     `json:"successes"`
	Failures  int32     `json:"failures"`
	Result    string    `json:"result,omitempty"` // success or failure
	Message   string    `json:"message,omitempty"`
	LastAt    time.Time `json:"lastAt"`
}

// clone copies the state deeply enough for a reader outside the worker.
// Guest is replaced, never changed in place, so it is shared.
func (st podState) clone() podState {
	st.Images = cloneMap(st.Images)
	st.Restarts = cloneMap(st.Restarts)
	st.Forwards = cloneMap(st.Forwards)
	if st.Containers != nil {
		cs := make(map[string]*ctrState, len(st.Containers))
		for k, v := range st.Containers {
			c := *v
			if v.Probes != nil {
				c.Probes = make(map[string]*probeState, len(v.Probes))
				for pk, pv := range v.Probes {
					p := *pv
					c.Probes[pk] = &p
				}
			}
			cs[k] = &c
		}
		st.Containers = cs
	}
	return st
}

func cloneMap[V any](m map[string]V) map[string]V {
	if m == nil {
		return nil
	}
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ctr returns the state of the named container, creating it.
func (st *podState) ctr(name string) *ctrState {
	if st.Containers == nil {
		st.Containers = map[string]*ctrState{}
	}
	c := st.Containers[name]
	if c == nil {
		c = &ctrState{}
		st.Containers[name] = c
	}
	return c
}

type waitingState struct {
	// Container names the container the reason is about; empty means all.
	Container string `json:"container,omitempty"`
	Reason    string `json:"reason"`
	Message   string `json:"message,omitempty"`
}

func podDir(root, uid string) string { return filepath.Join(root, uid) }

func loadPodState(root, uid string) (*podState, error) {
	b, err := os.ReadFile(filepath.Join(podDir(root, uid), "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st podState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func savePodState(root string, st *podState) error {
	dir := podDir(root, st.UID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "state.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "state.json"))
}
