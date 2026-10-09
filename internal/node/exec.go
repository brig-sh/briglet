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
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/streaming/pkg/httpstream/wsstream"

	"github.com/brig-sh/briglet/internal/hull"
)

// The remotecommand websocket channels, in the order the kubelet opens them.
const (
	chanStdin = iota
	chanStdout
	chanStderr
	chanError
	chanResize
)

const (
	protoV5 = "v5.channel.k8s.io"
	protoV4 = "v4.channel.k8s.io"
	// execIdleTimeout bounds a session with no traffic, as the kubelet's
	// streaming idle timeout does.
	execIdleTimeout = 4 * time.Hour
)

// exec runs a command in the pod's VM over a websocket. The apiserver
// passes kubectl's websocket through because the node declares
// ExtendWebSocketsToKubelet. v5.channel.k8s.io is preferred; v4 and the
// older channel protocols are accepted. A request that asks for SPDY goes
// to execSPDY.
func (s *kubeletServer) exec(w http.ResponseWriter, r *http.Request) {
	log.Printf("exec: %s %s upgrade=%q protocols=%q stream-protocol=%q",
		r.Method, r.URL.RequestURI(), r.Header.Get("Upgrade"),
		r.Header.Values("Sec-Websocket-Protocol"), r.Header.Values("X-Stream-Protocol-Version"))
	q := r.URL.Query()
	stdin, stdout, stderr, tty := q.Get("input") == "1", q.Get("output") == "1", q.Get("error") == "1", q.Get("tty") == "1"
	if tty {
		stderr = false
	}
	argv := q["command"]
	if len(argv) == 0 {
		http.Error(w, "missing command", http.StatusBadRequest)
		return
	}
	if !stdin && !stdout && !stderr {
		http.Error(w, "you must specify at least 1 of stdin, stdout, stderr", http.StatusBadRequest)
		return
	}
	_, st, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if !st.Running {
		http.Error(w, fmt.Sprintf("container %s is not running", st.Container), http.StatusBadRequest)
		return
	}
	if !wsstream.IsWebSocketRequest(r) {
		execSPDY(w, r, s.pm.hull.AgentSocket(st.Instance), r.PathValue("pod"), r.PathValue("uid"), st.Container, argv, st.Env)
		return
	}

	channel := func(on bool, t wsstream.ChannelType) wsstream.ChannelType {
		if on {
			return t
		}
		return wsstream.IgnoreChannel
	}
	channels := []wsstream.ChannelType{
		chanStdin:  channel(stdin, wsstream.ReadChannel),
		chanStdout: channel(stdout, wsstream.WriteChannel),
		chanStderr: channel(stderr, wsstream.WriteChannel),
		chanError:  wsstream.WriteChannel,
		chanResize: wsstream.ReadChannel,
	}
	binary := wsstream.ChannelProtocolConfig{Binary: true, Channels: channels}
	base64 := wsstream.ChannelProtocolConfig{Binary: false, Channels: channels}
	conn := wsstream.NewConn(map[string]wsstream.ChannelProtocolConfig{
		protoV5: binary,
		protoV4: binary,
		"v4." + wsstream.Base64ChannelWebSocketProtocol: base64,
		wsstream.ChannelWebSocketProtocol:               binary,
		wsstream.Base64ChannelWebSocketProtocol:         base64,
		"":                                              binary,
	})
	conn.SetIdleTimeout(execIdleTimeout)
	proto, streams, err := conn.Open(w, r)
	if err != nil {
		log.Printf("exec: websocket upgrade: %v", err)
		return
	}
	defer conn.Close()
	// An empty write on the first writable channel tells the client the
	// session is up.
	switch {
	case stdout:
		_, _ = streams[chanStdout].Write(nil)
	case stderr:
		_, _ = streams[chanStderr].Write(nil)
	default:
		_, _ = streams[chanError].Write(nil)
	}

	sizes := make(chan [2]uint16, 4)
	if tty {
		go func() {
			defer close(sizes)
			dec := json.NewDecoder(streams[chanResize])
			for {
				var ts struct{ Width, Height uint16 }
				if err := dec.Decode(&ts); err != nil {
					return
				}
				sizes <- [2]uint16{ts.Height, ts.Width}
			}
		}()
	}
	req := hull.OpenRequest{Argv: argv, Env: st.Env, TTY: tty, Rows: 24, Cols: 80}
	if tty {
		// kubectl sends the terminal size right after the upgrade.
		select {
		case sz, ok := <-sizes:
			if ok && sz[0] > 0 && sz[1] > 0 {
				req.Rows, req.Cols = sz[0], sz[1]
			}
		case <-time.After(300 * time.Millisecond):
		}
	}
	sess, err := hull.DialAgent(r.Context(), s.pm.hull.AgentSocket(st.Instance), req)
	if err != nil {
		writeExecStatus(streams[chanError], proto, -1, err)
		return
	}
	defer sess.Close()
	if tty {
		go func() {
			for sz := range sizes {
				_ = sess.Resize(sz[0], sz[1])
			}
		}()
	}
	if stdin {
		go func() { _ = sess.CopyStdin(streams[chanStdin]) }()
	} else {
		_ = sess.CopyStdin(strings.NewReader(""))
	}
	var out, errw io.Writer
	if stdout {
		out = streams[chanStdout]
	}
	if stderr {
		errw = streams[chanStderr]
	}
	code, err := sess.Wait(out, errw)
	log.Printf("exec: %s %q exited %d (protocol %q)", st.Instance, argv, code, proto)
	writeExecStatus(streams[chanError], proto, code, err)
}

// writeExecStatus reports the end of the command on the error channel: a
// metav1.Status for v4 and v5, a plain message for the older protocols.
func writeExecStatus(w io.Writer, proto string, code int, err error) {
	if proto != protoV5 && !strings.HasPrefix(proto, "v4.") {
		if err != nil {
			_, _ = w.Write([]byte(err.Error()))
		} else if code != 0 {
			_, _ = fmt.Fprintf(w, "command terminated with non-zero exit code: %d", code)
		}
		return
	}
	status := metav1.Status{Status: metav1.StatusSuccess}
	switch {
	case err != nil:
		status = metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonInternalError,
			Code: http.StatusInternalServerError, Message: "error executing command in container: " + err.Error()}
	case code != 0:
		status = metav1.Status{
			Status:  metav1.StatusFailure,
			Reason:  "NonZeroExitCode",
			Message: fmt.Sprintf("command terminated with non-zero exit code: exit status %d", code),
			Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{{Type: "ExitCode", Message: fmt.Sprint(code)}}},
		}
	}
	b, _ := json.Marshal(status)
	_, _ = w.Write(b)
}
