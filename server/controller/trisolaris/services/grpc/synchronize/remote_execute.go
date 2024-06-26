/*
 * Copyright (c) 2024 Yunshan Networks
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package synchronize

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"

	"github.com/deepflowio/deepflow/message/trident"
	api "github.com/deepflowio/deepflow/message/trident"
	"github.com/deepflowio/deepflow/server/controller/http/service"
	"google.golang.org/protobuf/proto"
)

func (e *VTapEvent) RemoteExecute(stream api.Synchronizer_RemoteExecuteServer) error {
	key := ""
	defer func() {
		if _, ok := service.AgentRemoteExecMap[key]; ok {
			delete(service.AgentRemoteExecMap, key)
			log.Infof("delete agent(key:%s) in manager", key)
		}
	}()

	manager := &service.CMDManager{}
	initDone := make(chan struct{})

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 2048)
				n := runtime.Stack(buf, false)
				log.Errorf("recovered in RemoteExecute: %s", buf[:n])
			}
		}()

		for {
			select {
			case <-ctx.Done():
				log.Infof("context done")
				return
			default:
				resp, err := stream.Recv()
				if resp == nil {
					continue
				}
				if resp.AgentId == nil {
					log.Warningf("recevie agent info from remote command is nil")
					continue
				}
				key = resp.AgentId.GetIp() + "-" + resp.AgentId.GetMac()
				if _, ok := service.AgentRemoteExecMap[key]; !ok {
					requestID := uint64(0)
					if resp.RequestId != nil {
						requestID = *resp.RequestId
					}
					manager = service.AddSteamToManager(key, requestID)
					log.Infof("add agent(key:%s) to cmd manager", key)
					initDone <- struct{}{}
				}
				// heartbeat
				if resp.CommandResult == nil && resp.LinuxNamespaces == nil &&
					resp.Commands == nil && resp.Errmsg == nil {
					manager.ExecCH <- &api.RemoteExecRequest{}
				}

				if err != nil {
					if err == io.EOF {
						handleResponse(resp)
						log.Infof("agent(key: %s) command exec get response finish", key)
						continue
					}

					err := fmt.Errorf("agent(key: %s) command stream error: %v", key, err)
					log.Error(err)
					continue
				}

				handleResponse(resp)
			}
		}
	}()

	<-initDone
	for {
		select {
		case <-ctx.Done():
			log.Infof("context done")
			return nil
		case req := <-manager.ExecCH:
			if req.ExecType != nil {
				req.RequestId = proto.Uint64(manager.GetRequestID() + 1)
			}
			b, _ := json.Marshal(req)
			log.Infof("agent(key: %s) request: %s", key, string(b))
			if err := stream.Send(req); err != nil {
				log.Errorf("send cmd to agent error: %s, req: %#v", err.Error(), req)
				return err
			}
		}
	}
}

func handleResponse(resp *trident.RemoteExecResponse) {
	key := resp.AgentId.GetIp() + "-" + resp.AgentId.GetMac()
	manager, ok := service.AgentRemoteExecMap[key]
	if !ok {
		log.Errorf("agent(key: %s) remote exec map not found", key)
		return
	}
	b, _ := json.Marshal(resp)
	log.Infof("agent(key: %s) resp: %s", key, string(b))

	if resp.RequestId != nil {
		manager.SetRequestID(*resp.RequestId)
	}

	switch {
	case len(resp.LinuxNamespaces) > 0:
		if len(manager.GetNamespaces()) > 0 {
			manager.InitNamespaces(resp.LinuxNamespaces)
		} else {
			manager.AppendNamespaces(resp.LinuxNamespaces)
		}
		manager.LinuxNamespaceDoneCH <- struct{}{}
	case len(resp.Commands) > 0:
		if len(manager.GetCommands()) > 0 {
			manager.InitCommands(resp.Commands)
		} else {
			manager.AppendCommands(resp.Commands)
		}
		manager.RemoteCMDDoneCH <- struct{}{}
	default:
		result := resp.CommandResult
		if resp.CommandResult == nil {
			return
		}

		if resp.Errmsg != nil {
			log.Errorf("agent(key: %s) run command error: %s",
				key, *resp.Errmsg)
			manager.AppendErr(resp.Errmsg)
			manager.ExecDoneCH <- struct{}{}
			return
		}
		if result.Content != nil {
			manager.AppendContent(result.Content)
		}
		if result.Md5 != nil {
			manager.ExecDoneCH <- struct{}{}
			return
		}
	}
}
