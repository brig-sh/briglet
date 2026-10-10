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

// Package guest is briglet-guest, the supervisor that runs a pod's
// containers inside its VM, and the contract between it and the node agent.
// This file builds on every platform: the agent writes pod.json and reads
// status.json with these types. The supervisor itself is Linux only.
package guest

import "time"

// Paths inside the VM. The agent shares the pod's guest directory read-write
// at PodDir and the directory holding the briglet-guest binary read-only at
// BinDir.
const (
	BinDir     = "/run/briglet-bin"
	Bin        = BinDir + "/briglet-guest"
	PodDir     = "/run/briglet"
	PodFile    = PodDir + "/pod.json"
	StatusFile = PodDir + "/status.json"
	CtlSock    = PodDir + "/ctl.sock"
	LogDir     = PodDir + "/logs"
	ImagesDir  = PodDir + "/images"
)

// Restart policies, as in the Pod spec.
const (
	RestartAlways    = "Always"
	RestartOnFailure = "OnFailure"
	RestartNever     = "Never"
)

// Pod is pod.json: everything the supervisor needs to run the pod.
type Pod struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	Hostname  string `json:"hostname"`
	// RestartPolicy is the pod's restartPolicy.
	RestartPolicy string `json:"restartPolicy"`
	// ShareProcessNamespace runs every container in the VM's pid
	// namespace instead of one of its own.
	ShareProcessNamespace bool `json:"shareProcessNamespace,omitempty"`
	// GraceSeconds is the pod's termination grace period, used when the
	// pod ends by itself and its sidecars stop.
	GraceSeconds int `json:"graceSeconds,omitempty"`
	// ShmMB sizes the pod's /dev/shm.
	ShmMB int `json:"shmMB,omitempty"`
	// Sysctls are written to /proc/sys before any container starts.
	Sysctls map[string]string `json:"sysctls,omitempty"`
	// Containers holds the init containers in order, then the regular
	// ones.
	Containers []Container `json:"containers"`
}

// Container is one container of the pod.
type Container struct {
	Name string `json:"name"`
	// Init marks an init container. RestartAlways on an init container
	// makes it a sidecar: started in order with the init containers and
	// kept running next to the regular ones.
	Init          bool `json:"init,omitempty"`
	RestartAlways bool `json:"restartAlways,omitempty"`
	// Rootfs is the guest path of the image's root filesystem, a read-only
	// share. The supervisor puts a tmpfs-backed overlay on top of it.
	Rootfs string   `json:"rootfs"`
	Argv   []string `json:"argv"`
	Env    []string `json:"env"`
	Cwd    string   `json:"cwd"`
	UID    uint32   `json:"uid"`
	GID    uint32   `json:"gid"`
	Groups []uint32 `json:"groups,omitempty"`
	// MemoryMB sizes the tmpfs that holds the container's writable layer.
	MemoryMB       int  `json:"memoryMB"`
	ReadOnlyRootfs bool `json:"readOnlyRootfs,omitempty"`
	// Privileged skips the masked and read-only /proc and /sys paths.
	Privileged bool `json:"privileged,omitempty"`
	// NoNewPrivs sets PR_SET_NO_NEW_PRIVS, so setuid binaries cannot
	// lift the container's user, as AllowPrivilegeEscalation false does.
	NoNewPrivs bool    `json:"noNewPrivs,omitempty"`
	Mounts     []Mount `json:"mounts,omitempty"`
	// TerminationLog is the guest path of the file bind-mounted at the
	// container's terminationMessagePath. FallbackToLogs uses the tail of
	// the log when that file is empty and the container failed.
	TerminationLog string `json:"terminationLog,omitempty"`
	FallbackToLogs bool   `json:"fallbackToLogs,omitempty"`
	// Lifecycle holds the container's lifecycle handlers.
	Lifecycle *Lifecycle `json:"lifecycle,omitempty"`
	// RestartCount is where the count starts: the restarts of the
	// container in earlier VMs of the pod.
	RestartCount int32 `json:"restartCount,omitempty"`
}

// Lifecycle runs a handler at a container's boundaries. PostStart runs
// right after the container starts; PreStop runs before the container's
// SIGTERM, inside the stop grace period.
type Lifecycle struct {
	PostStart *LifecycleHandler `json:"postStart,omitempty"`
	PreStop   *LifecycleHandler `json:"preStop,omitempty"`
}

// LifecycleHandler is one preStop action: a command inside the
// container, or an HTTP GET against the pod's address.
type LifecycleHandler struct {
	Exec []string `json:"exec,omitempty"`
	HTTP *HTTPGet `json:"http,omitempty"`
}

// HTTPGet targets the pod's address. Host defaults to 127.0.0.1, where
// every container of the pod answers, and Scheme to http.
type HTTPGet struct {
	Scheme string `json:"scheme,omitempty"`
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port"`
	Path   string `json:"path,omitempty"`
}

// Mount bind-mounts Source, a guest path, at Destination inside the
// container.
type Mount struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"readOnly,omitempty"`
	// Tmpfs mounts a tmpfs at Destination, as an emptyDir of Memory
	// medium: mode 0777, no Source. Volume names it: the supervisor mounts
	// one tmpfs per volume for the life of the VM, so the containers of
	// the pod share it and it survives a container restart.
	Tmpfs  bool   `json:"tmpfs,omitempty"`
	Volume string `json:"volume,omitempty"`
}

// Phases the supervisor reports for the pod.
const (
	PhaseInitializing = "Initializing"
	PhaseRunning      = "Running"
	PhaseSucceeded    = "Succeeded"
	PhaseFailed       = "Failed"
	PhaseStopping     = "Stopping"
)

// Container states.
const (
	StateWaiting    = "waiting"
	StateRunning    = "running"
	StateTerminated = "terminated"
)

// Status is status.json, written atomically by the supervisor on every
// change.
type Status struct {
	// Boot identifies one supervisor run, so the agent can tell a new VM's
	// status from a stale file.
	Boot      string    `json:"boot"`
	StartedAt time.Time `json:"startedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Phase     string    `json:"phase"`
	// Initialized is set once every init container has completed or, for
	// a sidecar, started.
	Initialized bool              `json:"initialized"`
	Message     string            `json:"message,omitempty"`
	Containers  []ContainerStatus `json:"containers"`
	// Final is set on the last write before the supervisor exits.
	Final bool `json:"final,omitempty"`
}

// ContainerStatus is the state of one container.
type ContainerStatus struct {
	Name         string       `json:"name"`
	State        string       `json:"state"`
	Reason       string       `json:"reason,omitempty"`
	Message      string       `json:"message,omitempty"`
	PID          int          `json:"pid,omitempty"`
	StartedAt    time.Time    `json:"startedAt,omitzero"`
	RestartCount int32        `json:"restartCount"`
	Term         *Termination `json:"term,omitempty"`
	Last         *Termination `json:"last,omitempty"`
}

// Termination describes a container run that ended.
type Termination struct {
	ExitCode   int32     `json:"exitCode"`
	Signal     int32     `json:"signal,omitempty"`
	Reason     string    `json:"reason"`
	Message    string    `json:"message,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt"`
}

// Find returns the status of the named container.
func (s *Status) Find(name string) *ContainerStatus {
	if s == nil {
		return nil
	}
	for i := range s.Containers {
		if s.Containers[i].Name == name {
			return &s.Containers[i]
		}
	}
	return nil
}

// Done reports whether the pod ended.
func (s *Status) Done() bool {
	return s != nil && (s.Phase == PhaseSucceeded || s.Phase == PhaseFailed)
}

// Ctl operations served on CtlSock.
const (
	// CtlRestart stops a container with Grace seconds and lets the restart
	// policy decide what follows, as a failed liveness probe does.
	CtlRestart = "restart"
	// CtlKill sends Signal to a container's process.
	CtlKill = "kill"
	// CtlStop stops the pod: every container with Grace seconds, then the
	// supervisor exits and the VM powers off.
	CtlStop = "stop"
)

// CtlRequest is one command on CtlSock, a JSON object per connection.
type CtlRequest struct {
	Op        string `json:"op"`
	Container string `json:"container,omitempty"`
	Signal    int    `json:"signal,omitempty"`
	Grace     int    `json:"grace,omitempty"`
}

// CtlResponse answers a CtlRequest.
type CtlResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
