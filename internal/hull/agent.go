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
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// The guest agent protocol, github.com/urunc-dev/urunc/pkg/agentproto. hull
// exec speaks it over <store>/instances/<id>/agent.sock. Every frame is a
// type byte, a big-endian uint32 stream id, a big-endian uint32 length and
// the payload. Control payloads are JSON, stdio payloads are raw bytes.
const (
	frameOpen       byte = 1
	frameStdin      byte = 2
	frameStdout     byte = 3
	frameStderr     byte = 4
	frameCloseStdin byte = 5
	frameResize     byte = 6
	frameSignal     byte = 7
	frameExit       byte = 8
	frameError      byte = 9

	maxFramePayload = 1 << 20
	// MaxStdinChunk bounds one stdin frame. The agent stalled on frames above
	// about 3.7 KB once (brig-sh/hull#82); brig caps its feeds at 2 KB too.
	MaxStdinChunk = 2048
	// One session per connection; hull exec uses stream 1 the same way.
	agentStream = 1
)

// OpenRequest starts a process in the guest.
type OpenRequest struct {
	Argv []string `json:"argv"`
	Env  []string `json:"env,omitempty"`
	Cwd  string   `json:"cwd,omitempty"`
	User string   `json:"user,omitempty"`
	TTY  bool     `json:"tty"`
	Rows uint16   `json:"rows,omitempty"`
	Cols uint16   `json:"cols,omitempty"`
}

// AgentSession is one process running in the guest.
type AgentSession struct {
	conn net.Conn
	wmu  sync.Mutex
}

// DialAgent connects to the agent socket and opens a session. The socket
// belongs to the VMM and accepts before the guest agent listens, so a caller
// right after boot should retry on failure.
func DialAgent(ctx context.Context, sock string, req OpenRequest) (*AgentSession, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, fmt.Errorf("guest agent %s: %w", sock, err)
	}
	s := &AgentSession{conn: conn}
	if err := s.writeJSON(frameOpen, req); err != nil {
		conn.Close()
		return nil, fmt.Errorf("guest agent open: %w", err)
	}
	return s, nil
}

func (s *AgentSession) writeFrame(typ byte, payload []byte) error {
	if len(payload) > maxFramePayload {
		return fmt.Errorf("frame payload too large: %d", len(payload))
	}
	var hdr [9]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:5], agentStream)
	binary.BigEndian.PutUint32(hdr[5:9], uint32(len(payload)))
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if _, err := s.conn.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := s.conn.Write(payload)
		return err
	}
	return nil
}

func (s *AgentSession) writeJSON(typ byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.writeFrame(typ, b)
}

// Resize sets the window size of a tty session.
func (s *AgentSession) Resize(rows, cols uint16) error {
	return s.writeJSON(frameResize, struct {
		Rows uint16 `json:"rows"`
		Cols uint16 `json:"cols"`
	}{rows, cols})
}

// Signal delivers a signal to the process.
func (s *AgentSession) Signal(sig int) error {
	return s.writeJSON(frameSignal, struct {
		Signal int `json:"signal"`
	}{sig})
}

// CopyStdin feeds r to the process in chunks of at most MaxStdinChunk bytes
// and closes the process's stdin at EOF.
func (s *AgentSession) CopyStdin(r io.Reader) error {
	buf := make([]byte, MaxStdinChunk)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if err := s.writeFrame(frameStdin, buf[:n]); err != nil {
				return err
			}
		}
		if rerr != nil {
			_ = s.writeFrame(frameCloseStdin, nil)
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}

// Wait relays output until the process exits and returns its exit code.
// A nil writer drops that stream.
func (s *AgentSession) Wait(stdout, stderr io.Writer) (int, error) {
	for {
		var hdr [9]byte
		if _, err := io.ReadFull(s.conn, hdr[:]); err != nil {
			return -1, fmt.Errorf("connection to guest agent lost: %w", err)
		}
		n := binary.BigEndian.Uint32(hdr[5:9])
		if n > maxFramePayload {
			return -1, fmt.Errorf("guest agent frame too large: %d", n)
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(s.conn, payload); err != nil {
			return -1, fmt.Errorf("connection to guest agent lost: %w", err)
		}
		switch hdr[0] {
		case frameStdout:
			if stdout != nil {
				_, _ = stdout.Write(payload)
			}
		case frameStderr:
			if stderr != nil {
				_, _ = stderr.Write(payload)
			}
		case frameExit:
			var ex struct {
				Code int `json:"code"`
			}
			_ = json.Unmarshal(payload, &ex)
			return ex.Code, nil
		case frameError:
			var ae struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(payload, &ae)
			return -1, fmt.Errorf("guest agent: %s", ae.Message)
		}
	}
}

// Close ends the session.
func (s *AgentSession) Close() error { return s.conn.Close() }
