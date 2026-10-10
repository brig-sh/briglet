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

//go:build linux

package guest

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"time"
)

// serveCtl listens on CtlSock and hands each request to the main loop.
func (s *supervisor) serveCtl() error {
	_ = os.Remove(CtlSock)
	ln, err := net.Listen("unix", CtlSock)
	if err != nil {
		return err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				log.Printf("ctl: accept: %v", err)
				return
			}
			go s.handleCtl(conn)
		}
	}()
	return nil
}

func (s *supervisor) handleCtl(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	var req CtlRequest
	resp := CtlResponse{}
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		resp.Error = "bad request: " + err.Error()
	} else {
		reply := make(chan CtlResponse, 1)
		s.ctl <- ctlCall{req: req, reply: reply}
		resp = <-reply
	}
	_ = json.NewEncoder(conn).Encode(resp)
}

// Ctl sends one request to the supervisor of this VM.
func Ctl(req CtlRequest) error {
	conn, err := net.DialTimeout("unix", CtlSock, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	var resp CtlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return fmt.Errorf("ctl reply: %w", err)
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	return nil
}
