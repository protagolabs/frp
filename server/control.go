// Copyright 2017 fatedier, fatedier@gmail.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/fatedier/golib/crypto"
	"github.com/google/uuid"
	"io"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	er "errors"
	"github.com/fatedier/frp/pkg/auth"
	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/consts"
	frpErr "github.com/fatedier/frp/pkg/errors"
	"github.com/fatedier/frp/pkg/msg"
	plugin "github.com/fatedier/frp/pkg/plugin/server"
	"github.com/fatedier/frp/pkg/util/util"
	"github.com/fatedier/frp/pkg/util/version"
	"github.com/fatedier/frp/pkg/util/xlog"
	"github.com/fatedier/frp/server/controller"
	"github.com/fatedier/frp/server/metrics"
	"github.com/fatedier/frp/server/proxy"
	"github.com/fatedier/golib/control/shutdown"
	"github.com/fatedier/golib/errors"
)

type ControlManager struct {
	// controls indexed by run id
	ctlsByRunID map[string]*Control

	mu sync.RWMutex
}

func NewControlManager() *ControlManager {
	return &ControlManager{
		ctlsByRunID: make(map[string]*Control),
	}
}

func (cm *ControlManager) Add(runID string, ctl *Control) (oldCtl *Control) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	oldCtl, ok := cm.ctlsByRunID[runID]
	if ok {
		oldCtl.Replaced(ctl)
	}
	cm.ctlsByRunID[runID] = ctl
	return
}

// we should make sure if it's the same control to prevent delete a new one
func (cm *ControlManager) Del(runID string, ctl *Control) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if c, ok := cm.ctlsByRunID[runID]; ok && c == ctl {
		delete(cm.ctlsByRunID, runID)
	}
}

func (cm *ControlManager) GetByID(runID string) (ctl *Control, ok bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	ctl, ok = cm.ctlsByRunID[runID]
	return
}

type Control struct {
	// all resource managers and controllers
	rc *controller.ResourceController

	// proxy manager
	pxyManager *proxy.Manager

	// plugin manager
	pluginManager *plugin.Manager

	// verifies authentication based on selected method
	authVerifier auth.Verifier

	// login message
	loginMsg *msg.Login

	// control connection
	conn net.Conn

	// put a message in this channel to send it over control connection to client
	sendCh chan (msg.Message)

	// read from this channel to get the next message sent by client
	readCh chan (msg.Message)

	// work connections
	workConnCh chan net.Conn

	// proxies in one client
	proxies map[string]proxy.Proxy

	// pool count
	poolCount int

	// ports used, for limitations
	portsUsedNum int

	// last time got the Ping message
	lastPing time.Time

	// A new run id will be generated when a new client login.
	// If run id got from login message has same run id, it means it's the same client, so we can
	// replace old controller instantly.
	runID string

	// control status
	status string

	readerShutdown  *shutdown.Shutdown
	writerShutdown  *shutdown.Shutdown
	managerShutdown *shutdown.Shutdown
	allShutdown     *shutdown.Shutdown

	mu sync.RWMutex

	// Server configuration information
	serverCfg config.ServerCommonConf

	xl              *xlog.Logger
	ctx             context.Context
	closed          bool
	ServerCreateUrl string
	ServerDeleteUrl string
}

func NewControl(
	ctx context.Context,
	rc *controller.ResourceController,
	pxyManager *proxy.Manager,
	pluginManager *plugin.Manager,
	authVerifier auth.Verifier,
	ctlConn net.Conn,
	loginMsg *msg.Login,
	serverCfg config.ServerCommonConf,
) *Control {
	poolCount := loginMsg.PoolCount
	if poolCount > int(serverCfg.MaxPoolCount) {
		poolCount = int(serverCfg.MaxPoolCount)
	}
	stage := os.Getenv("STAGE")
	var ServerCreateUrl string
	var ServerDeleteUrl string
	// use address in eks , due to its been deployed in same cluster
	if stage == "prod" {
		ServerCreateUrl = "http://service-auto-deploy.gradio-inference-deploy.svc.cluster.local:8600/endpoint/create"
		ServerDeleteUrl = "http://service-auto-deploy.gradio-inference-deploy.svc.cluster.local:8600/endpoint/delete_by_url"
	} else {
		ServerCreateUrl = "http://service-auto-deploy-test.gradio-inference-deploy.svc.cluster.local:8600/endpoint/create"
		ServerDeleteUrl = "http://service-auto-deploy-test.gradio-inference-deploy.svc.cluster.local:8600/endpoint/delete_by_url"
	}
	return &Control{
		rc:              rc,
		pxyManager:      pxyManager,
		pluginManager:   pluginManager,
		authVerifier:    authVerifier,
		conn:            ctlConn,
		loginMsg:        loginMsg,
		sendCh:          make(chan msg.Message, 10),
		readCh:          make(chan msg.Message, 10),
		workConnCh:      make(chan net.Conn, poolCount+10),
		proxies:         make(map[string]proxy.Proxy),
		poolCount:       poolCount,
		portsUsedNum:    0,
		lastPing:        time.Now(),
		runID:           loginMsg.RunID,
		status:          consts.Working,
		readerShutdown:  shutdown.New(),
		writerShutdown:  shutdown.New(),
		managerShutdown: shutdown.New(),
		allShutdown:     shutdown.New(),
		serverCfg:       serverCfg,
		xl:              xlog.FromContextSafe(ctx),
		ctx:             ctx,
		closed:          false,
		ServerCreateUrl: ServerCreateUrl,
		ServerDeleteUrl: ServerDeleteUrl,
	}
}

// Start send a login success message to client and start working.
func (ctl *Control) Start() {
	loginRespMsg := &msg.LoginResp{
		Version:       version.Full(),
		RunID:         ctl.runID,
		ServerUDPPort: ctl.serverCfg.BindUDPPort,
		Error:         "",
	}
	_ = msg.WriteMsg(ctl.conn, loginRespMsg)

	go ctl.writer()
	for i := 0; i < ctl.poolCount; i++ {
		ctl.sendCh <- &msg.ReqWorkConn{}
	}

	go ctl.manager()
	go ctl.reader()
	go ctl.stoper()
	//go ctl.cleaner()
}

func (ctl *Control) cleaner() {
	time.Sleep(time.Hour * 24 * 3)
	if !ctl.closed {
		ctl.sendCh <- &msg.Kill{}
	}
}

func (ctl *Control) RegisterWorkConn(conn net.Conn) error {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	select {
	case ctl.workConnCh <- conn:
		xl.Debug("new work connection registered")
		return nil
	default:
		xl.Debug("work connection pool is full, discarding")
		return fmt.Errorf("work connection pool is full, discarding")
	}
}

// When frps get one user connection, we get one work connection from the pool and return it.
// If no workConn available in the pool, send message to frpc to get one or more
// and wait until it is available.
// return an error if wait timeout
func (ctl *Control) GetWorkConn() (workConn net.Conn, err error) {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	var ok bool
	// get a work connection from the pool
	select {
	case workConn, ok = <-ctl.workConnCh:
		if !ok {
			err = frpErr.ErrCtlClosed
			return
		}
		xl.Debug("get work connection from pool")
	default:
		// no work connections available in the poll, send message to frpc to get more
		if err = errors.PanicToError(func() {
			ctl.sendCh <- &msg.ReqWorkConn{}
		}); err != nil {
			return nil, fmt.Errorf("control is already closed")
		}

		select {
		case workConn, ok = <-ctl.workConnCh:
			if !ok {
				err = frpErr.ErrCtlClosed
				xl.Warn("no work connections available, %v", err)
				return
			}

		case <-time.After(time.Duration(ctl.serverCfg.UserConnTimeout) * time.Second):
			err = fmt.Errorf("timeout trying to get work connection")
			xl.Warn("%v", err)
			return
		}
	}

	// When we get a work connection from pool, replace it with a new one.
	_ = errors.PanicToError(func() {
		ctl.sendCh <- &msg.ReqWorkConn{}
	})
	return
}

func (ctl *Control) Replaced(newCtl *Control) {
	xl := ctl.xl
	xl.Info("Replaced by client [%s]", newCtl.runID)
	ctl.runID = ""
	ctl.allShutdown.Start()
}

func (ctl *Control) writer() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	defer ctl.allShutdown.Start()
	defer ctl.writerShutdown.Done()

	encWriter, err := crypto.NewWriter(ctl.conn, []byte(ctl.serverCfg.Token))
	if err != nil {
		xl.Error("crypto new writer error: %v", err)
		ctl.allShutdown.Start()
		return
	}
	for {
		m, ok := <-ctl.sendCh
		if !ok {
			xl.Info("control writer is closing")
			return
		}

		if err := msg.WriteMsg(encWriter, m); err != nil {
			xl.Warn("write message to control connection error: %v", err)
			return
		}
	}
}

func (ctl *Control) reader() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	defer ctl.allShutdown.Start()
	defer ctl.readerShutdown.Done()

	encReader := crypto.NewReader(ctl.conn, []byte(ctl.serverCfg.Token))
	for {
		m, err := msg.ReadMsg(encReader)
		if err != nil {
			if err == io.EOF {
				xl.Debug("control connection closed")
				return
			}
			xl.Warn("read error: %v", err)
			ctl.conn.Close()
			return
		}

		ctl.readCh <- m
	}
}

func (ctl *Control) stoper() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	ctl.allShutdown.WaitStart()
	ctl.closed = true

	ctl.conn.Close()
	ctl.readerShutdown.WaitDone()

	close(ctl.readCh)
	ctl.managerShutdown.WaitDone()

	close(ctl.sendCh)
	ctl.writerShutdown.WaitDone()

	ctl.mu.Lock()
	defer ctl.mu.Unlock()

	close(ctl.workConnCh)
	for workConn := range ctl.workConnCh {
		workConn.Close()
	}

	for _, pxy := range ctl.proxies {
		pxy.Close()
		ctl.pxyManager.Del(pxy.GetName())
		metrics.Server.CloseProxy(pxy.GetName(), pxy.GetConf().GetBaseInfo().ProxyType)

		notifyContent := &plugin.CloseProxyContent{
			User: plugin.UserInfo{
				User:  ctl.loginMsg.User,
				Metas: ctl.loginMsg.Metas,
				RunID: ctl.loginMsg.RunID,
			},
			CloseProxy: msg.CloseProxy{
				ProxyName: pxy.GetName(),
			},
		}
		go func() {
			_ = ctl.pluginManager.CloseProxy(notifyContent)
		}()
		go ctl.SendDeleteEndpoint(pxy.GetName())
	}
	ctl.allShutdown.Done()
	xl.Info("client exit success")
	metrics.Server.CloseClient()
}

// block until Control closed
func (ctl *Control) WaitClosed() {
	ctl.allShutdown.WaitDone()
}

func (ctl *Control) manager() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	defer ctl.allShutdown.Start()
	defer ctl.managerShutdown.Done()

	var heartbeatCh <-chan time.Time
	if ctl.serverCfg.TCPMux || ctl.serverCfg.HeartbeatTimeout <= 0 {
		// Don't need application heartbeat here.
		// yamux will do same thing.
	} else {
		heartbeat := time.NewTicker(time.Second)
		defer heartbeat.Stop()
		heartbeatCh = heartbeat.C
	}

	for {
		select {
		case <-heartbeatCh:
			if time.Since(ctl.lastPing) > time.Duration(ctl.serverCfg.HeartbeatTimeout)*time.Second {
				xl.Warn("heartbeat timeout")
				return
			}
		case rawMsg, ok := <-ctl.readCh:
			if !ok {
				return
			}

			switch m := rawMsg.(type) {
			case *msg.NewProxy:
				content := &plugin.NewProxyContent{
					User: plugin.UserInfo{
						User:  ctl.loginMsg.User,
						Metas: ctl.loginMsg.Metas,
						RunID: ctl.loginMsg.RunID,
					},
					NewProxy: *m,
				}
				var remoteAddr string
				retContent, err := ctl.pluginManager.NewProxy(content)
				if err == nil {
					m = &retContent.NewProxy

					//modelName := os.Getenv("MODEL_NAME")
					//var modelNameSuffix string
					//if modelName != "" {
					//	modelNameSuffix = "-" + modelName
					//} else {
					//	modelNameSuffix = ""
					//}

					if m.ProxyName != "random" {
						fmt.Println("use sha256..", m.ProxyName)
						parts := strings.Split(m.ProxyName, "||")
						if len(parts) != 2 {
							fmt.Println("ProxyName should have || , use gradio with netmind version")
							err := er.New("ProxyName should have || , use gradio with netmind version")
							panic(err)
						}
						shareToken := parts[0]
						modelName := "-" + parts[1]
						h := sha256.New()
						h.Write([]byte(shareToken))
						bs := h.Sum(nil)
						m.ProxyName = fmt.Sprintf("%x", bs)[:18] + modelName
					} else {
						fmt.Println("use uuid..", m.ProxyName)
						m.ProxyName = uuid.NewString()[:18]
					}
					remoteAddr, err = ctl.RegisterProxy(m)
				}
				//
				//	if m.ProxyName != "random" {
				//		h := sha256.New()
				//		h.Write([]byte(m.ProxyName))
				//		bs := h.Sum(nil)
				//		m.ProxyName = fmt.Sprintf("%x", bs)[:18]
				//	} else {
				//		m.ProxyName = uuid.NewString()[:18]
				//	}
				//	remoteAddr, err = ctl.RegisterProxy(m)
				//}

				// register proxy in this control
				resp := &msg.NewProxyResp{
					ProxyName: m.ProxyName,
				}
				if err != nil {
					xl.Warn("new proxy [%s] type [%s] error: %v", m.ProxyName, m.ProxyType, err)
					resp.Error = util.GenerateResponseErrorString(fmt.Sprintf("new proxy [%s] error", m.ProxyName), err, ctl.serverCfg.DetailedErrorsToClient)
				} else {
					resp.RemoteAddr = remoteAddr
					xl.Info("new proxy [%s] type [%s] success", m.ProxyName, m.ProxyType)
					metrics.Server.NewProxy(m.ProxyName, m.ProxyType)
				}
				ctl.sendCh <- resp
				if m.ProxyType == "http" {
					go ctl.SendCreateEndpoint(m.ProxyName)
				}
			case *msg.CloseProxy:
				_ = ctl.CloseProxy(m)
				xl.Info("close proxy [%s] success", m.ProxyName)
			case *msg.Ping:
				content := &plugin.PingContent{
					User: plugin.UserInfo{
						User:  ctl.loginMsg.User,
						Metas: ctl.loginMsg.Metas,
						RunID: ctl.loginMsg.RunID,
					},
					Ping: *m,
				}
				retContent, err := ctl.pluginManager.Ping(content)
				if err == nil {
					m = &retContent.Ping
					err = ctl.authVerifier.VerifyPing(m)
				}
				if err != nil {
					xl.Warn("received invalid ping: %v", err)
					ctl.sendCh <- &msg.Pong{
						Error: util.GenerateResponseErrorString("invalid ping", err, ctl.serverCfg.DetailedErrorsToClient),
					}
					return
				}
				ctl.lastPing = time.Now()
				//xl.Debug("receive heartbeat")
				ctl.sendCh <- &msg.Pong{}
			}
		}
	}
}
func (ctl *Control) SendDeleteEndpoint(prefix string) {
	connectUrl := fmt.Sprintf("%s.%s", prefix, ctl.serverCfg.SubDomainHost)
	fmt.Println("debug:  send connectUrl:", connectUrl)
	data, err := json.Marshal(map[string]string{"endpoint_url": connectUrl})
	if err != nil {
		ctl.xl.Error("json marshal error")
		fmt.Println("error in json marshal")
		panic(err)
	}
	fmt.Println("Sending JSON data:", string(data))

	response, err := http.Post(ctl.ServerDeleteUrl, "application/json", bytes.NewBuffer(data))
	defer func() {
		if response != nil {
			response.Body.Close()
		}
	}()
	if err != nil {
		ctl.xl.Error("HTTP post error")
		fmt.Println("error in HTTP post:", err)
		return
	}
	var result map[string]string
	if body, err := io.ReadAll(response.Body); err == nil {
		err = json.Unmarshal(body, &result)
	}
	fmt.Println("delete endpoint result:", result)
}
func (ctl *Control) SendCreateEndpoint(prefix string) {

	connectUrl := fmt.Sprintf("%s.%s", prefix, ctl.serverCfg.SubDomainHost)
	fmt.Println("debug:  send connectUrl:", connectUrl)
	data, err := json.Marshal(map[string]string{"endpoint_url": connectUrl})
	if err != nil {
		ctl.xl.Error("json marshal error")
		fmt.Println("error in json marshal")
		panic(err)
	}
	fmt.Println("Sending JSON data:", string(data))
	response, err := http.Post(ctl.ServerCreateUrl, "application/json", bytes.NewBuffer(data))
	defer func() {
		if response != nil {
			response.Body.Close()
		}
	}()
	if err != nil {
		ctl.xl.Error("HTTP post error")
		fmt.Println("error in HTTP post:", err)
		return
	}

	var result map[string]string
	if body, err := io.ReadAll(response.Body); err == nil {
		err = json.Unmarshal(body, &result)
	}
	fmt.Println("create endpoint result:", result)
}

func (ctl *Control) RegisterProxy(pxyMsg *msg.NewProxy) (remoteAddr string, err error) {
	var pxyConf config.ProxyConf
	// Load configures from NewProxy message and check.
	pxyConf, err = config.NewProxyConfFromMsg(pxyMsg, ctl.serverCfg)
	if err != nil {
		return
	}

	// User info
	userInfo := plugin.UserInfo{
		User:  ctl.loginMsg.User,
		Metas: ctl.loginMsg.Metas,
		RunID: ctl.runID,
	}

	// NewProxy will return a interface Proxy.
	// In fact it create different proxies by different proxy type, we just call run() here.
	pxy, err := proxy.NewProxy(ctl.ctx, userInfo, ctl.rc, ctl.poolCount, ctl.GetWorkConn, pxyConf, ctl.serverCfg)
	if err != nil {
		return remoteAddr, err
	}

	// Check ports used number in each client
	if ctl.serverCfg.MaxPortsPerClient > 0 {
		ctl.mu.Lock()
		if ctl.portsUsedNum+pxy.GetUsedPortsNum() > int(ctl.serverCfg.MaxPortsPerClient) {
			ctl.mu.Unlock()
			err = fmt.Errorf("exceed the max_ports_per_client")
			return
		}
		ctl.portsUsedNum += pxy.GetUsedPortsNum()
		ctl.mu.Unlock()

		defer func() {
			if err != nil {
				ctl.mu.Lock()
				ctl.portsUsedNum -= pxy.GetUsedPortsNum()
				ctl.mu.Unlock()
			}
		}()
	}

	remoteAddr, err = pxy.Run()
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			pxy.Close()
		}
	}()

	err = ctl.pxyManager.Add(pxyMsg.ProxyName, pxy)
	if err != nil {
		return
	}

	ctl.mu.Lock()
	ctl.proxies[pxy.GetName()] = pxy
	ctl.mu.Unlock()
	return
}

func (ctl *Control) CloseProxy(closeMsg *msg.CloseProxy) (err error) {
	ctl.mu.Lock()
	pxy, ok := ctl.proxies[closeMsg.ProxyName]
	if !ok {
		ctl.mu.Unlock()
		return
	}

	if ctl.serverCfg.MaxPortsPerClient > 0 {
		ctl.portsUsedNum -= pxy.GetUsedPortsNum()
	}
	pxy.Close()
	ctl.pxyManager.Del(pxy.GetName())
	delete(ctl.proxies, closeMsg.ProxyName)
	ctl.mu.Unlock()

	metrics.Server.CloseProxy(pxy.GetName(), pxy.GetConf().GetBaseInfo().ProxyType)

	notifyContent := &plugin.CloseProxyContent{
		User: plugin.UserInfo{
			User:  ctl.loginMsg.User,
			Metas: ctl.loginMsg.Metas,
			RunID: ctl.loginMsg.RunID,
		},
		CloseProxy: msg.CloseProxy{
			ProxyName: pxy.GetName(),
		},
	}
	go func() {
		_ = ctl.pluginManager.CloseProxy(notifyContent)
	}()

	return
}
