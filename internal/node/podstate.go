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
)

// podState is what the agent knows about one pod beyond the API object. It
// is persisted in <state>/pods/<uid>/state.json so a restarted agent adopts
// the running VM instead of booting another.
type podState struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Container string `json:"container"`
	Instance  string `json:"instance"`
	IP        string `json:"ip,omitempty"`
	// Env is the container environment, also given to kubectl exec.
	Env     []string `json:"env,omitempty"`
	Image   string   `json:"image,omitempty"`
	ImageID string   `json:"imageID,omitempty"`

	RestartCount int32     `json:"restartCount"`
	Running      bool      `json:"running"`
	StartedAt    time.Time `json:"startedAt,omitempty"`
	// Created is set once a VM for the pod has existed.
	Created bool `json:"created,omitempty"`
	// Done is set when the pod reached Succeeded or Failed and will not run
	// again.
	Done bool `json:"done,omitempty"`
	// Rejected holds why the pod cannot run on briglet.
	Rejected string `json:"rejected,omitempty"`

	// Started is set once the startup probe passed, or at start without
	// one. Ready is the readiness the probes give the running container.
	Started bool `json:"started,omitempty"`
	Ready   bool `json:"ready,omitempty"`
	// Probes holds the last result and the consecutive counts of each
	// probe of the running container, keyed by kind, so an agent restart
	// does not reset them.
	Probes map[string]*probeState `json:"probes,omitempty"`
	// Forwards maps a container port to the 127.0.0.1 address the gateway
	// forwards to it. httpGet probes use them.
	Forwards map[string]string `json:"forwards,omitempty"`
	// TokenRefreshAt is when the next service account token in a volume is
	// due. Zero when no volume holds one.
	TokenRefreshAt time.Time `json:"tokenRefreshAt,omitempty"`

	Waiting  *waitingState `json:"waiting,omitempty"`
	LastTerm *termState    `json:"lastTerm,omitempty"`
	// Term is the current terminated state of a container that will not
	// restart (Done) or waits for a restart (back-off).
	Term *termState `json:"term,omitempty"`
}

type probeState struct {
	Successes int32     `json:"successes"`
	Failures  int32     `json:"failures"`
	Result    string    `json:"result,omitempty"` // success or failure
	Message   string    `json:"message,omitempty"`
	LastAt    time.Time `json:"lastAt"`
}

// clone copies the state deeply enough for a reader outside the worker.
func (st podState) clone() podState {
	st.Env = append([]string(nil), st.Env...)
	if st.Probes != nil {
		probes := make(map[string]*probeState, len(st.Probes))
		for k, v := range st.Probes {
			c := *v
			probes[k] = &c
		}
		st.Probes = probes
	}
	if st.Forwards != nil {
		fw := make(map[string]string, len(st.Forwards))
		for k, v := range st.Forwards {
			fw[k] = v
		}
		st.Forwards = fw
	}
	return st
}

type waitingState struct {
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

type termState struct {
	ExitCode   int32     `json:"exitCode"`
	Reason     string    `json:"reason"`
	Message    string    `json:"message,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
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
