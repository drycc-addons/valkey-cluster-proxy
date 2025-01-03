package proxy

import (
	"bufio"
	"bytes"
	"container/heap"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	resp "github.com/drycc-addons/valkey-cluster-proxy/proto"
	"github.com/golang/glog"
)

var (
	OK              = []byte("OK")
	MOVED           = []byte("-MOVED")
	ASK             = []byte("-ASK")
	ASK_CMD_BYTES   = []byte("+ASKING\r\n")
	AUTH_CMD_ERR    = []byte("ERR invalid password")
	UNKNOWN_CMD_ERR = []byte("ERR unknown command")
	ARGUMENTS_ERR   = []byte("ERR wrong number of arguments")
	NOAUTH_ERR      = []byte("NOAUTH Authentication required.")
	OK_DATA         = &resp.Data{T: resp.T_SimpleString, String: OK}
)

type Session struct {
	net.Conn
	r           *bufio.Reader
	auth        bool
	reqSeq      int64
	rspSeq      int64
	backQ       chan *PipelineResponse
	closed      bool
	cached      map[string]map[string]string
	closeSignal *sync.WaitGroup
	reqWg       *sync.WaitGroup
	rspHeap     *PipelineResponseHeap
	valkeyConn  *ValkeyConn
	dispatcher  *Dispatcher
	multiCmd    *[]*resp.Command
	multiCmdErr bool
}

func (s *Session) Prepare() {
	s.closeSignal.Add(1)
}

// WritingLoop consumes backQ and send response to client
// It close the connection to notify reader on error
// and continue loop until the reader has exited
func (s *Session) WritingLoop() {
	for rsp := range s.backQ {
		if err := s.handleRespPipeline(rsp); err != nil {
			s.Close()
			continue
		}
	}
	defer s.Close()
	defer s.closeSignal.Done()
}

func (s *Session) checkAuth() bool {
	return s.auth || s.valkeyConn.Auth("")
}

func (s *Session) ReadingLoop() {
	for {
		cmd, err := resp.ReadCommand(s.r)
		if err != nil {
			glog.V(2).Info(err)
			break
		}
		// convert all command name to upper case
		cmd.Args[0] = strings.ToUpper(cmd.Args[0])

		if len(cmd.Args) > 1 {
			glog.Infof("access %s %s %s", s.RemoteAddr(), cmd.Name(), cmd.Args[1])
		} else {
			glog.Infof("access %s %s", s.RemoteAddr(), cmd.Name())
		}
		s.handle(cmd)
	}
	// wait for all request done
	s.reqWg.Wait()
	// notify writer
	close(s.backQ)
	s.closeSignal.Wait()
}

func (s *Session) handle(cmd *resp.Command) {
	if CmdAuthRequired(cmd) && !s.checkAuth() {
		s.handleErrorCmd(NOAUTH_ERR)
	} else if cmd.Name() == "MULTI" || s.multiCmd != nil || cmd.Name() == "EXEC" {
		s.handleMultiCmd(cmd)
	} else if cmd.Name() == "AUTH" {
		s.handleAuthCmd(cmd)
	} else if cmd.Name() == "SELECT" {
		s.handleSimpleStringCmd(OK)
	} else if cmd.Name() == "PING" {
		s.handleSimpleStringCmd([]byte("PONG"))
	} else if CmdUnknown(cmd) {
		s.handleErrorCmd(UNKNOWN_CMD_ERR)
	} else if CmdReadAll(cmd) {
		s.handleReadAll(cmd)
	} else if yes, numKeys := IsMultiCmd(cmd); yes && numKeys > 1 {
		s.handleMultiKeyCmd(cmd, numKeys)
	} else { // other general cmd
		s.handleGeneralCmd(cmd)
	}
}

// 将resp写出去。如果是multi key command，只有在全部完成后才汇总输出
func (s *Session) writeResp(plRsp *PipelineResponse) error {
	var buf []byte
	if parentCmd := plRsp.ctx.parentCmd; parentCmd != nil {
		// sub request
		parentCmd.OnSubCmdFinished(plRsp)
		if !parentCmd.Finished() {
			return nil
		}
		buf = parentCmd.CoalesceRsp().rsp.Raw()
		s.rspSeq++
	} else {
		buf = plRsp.rsp.Raw()
	}
	// write to client directly with non-buffered io
	if _, err := s.Write(buf); err != nil {
		glog.Error(err)
		return err
	}

	return nil
}

// redirect send request to backend again to new server told by valkey cluster
func (s *Session) redirect(server string, plRsp *PipelineResponse, ask bool) {
	var conn net.Conn
	var err error

	plRsp.err = nil
	conn, err = s.valkeyConn.Conn(server)
	if err != nil {
		glog.Error(err)
		plRsp.err = err
		return
	}
	defer func() {
		if err != nil {
			glog.Error(err)
		}
		conn.Close()
	}()

	reader := bufio.NewReader(conn)
	if ask {
		if _, err = conn.Write(ASK_CMD_BYTES); err != nil {
			plRsp.err = err
			return
		}
	}
	if _, err = conn.Write(plRsp.ctx.cmd.Format()); err != nil {
		plRsp.err = err
		return
	}
	if ask {
		if _, err = resp.ReadData(reader); err != nil {
			plRsp.err = err
			return
		}
	}
	obj := resp.NewObject()
	if err = resp.ReadDataBytes(reader, obj); err != nil {
		plRsp.err = err
	} else {
		plRsp.rsp = obj
	}
}

// handleResp handles MOVED and ASK redirection and call write response
func (s *Session) handleResp(plRsp *PipelineResponse) error {
	if plRsp.ctx.seq != s.rspSeq {
		panic("impossible")
	}
	plRsp.ctx.wg.Done()
	if plRsp.ctx.parentCmd == nil {
		s.rspSeq++
	}

	if plRsp.err != nil {
		s.dispatcher.TriggerReloadSlots()
		rsp := &resp.Data{T: resp.T_Error, String: []byte(plRsp.err.Error())}
		plRsp.rsp = resp.NewObjectFromData(rsp)
	} else {
		raw := plRsp.rsp.Raw()
		if raw[0] == resp.T_Error {
			if bytes.HasPrefix(raw, MOVED) {
				_, server := ParseRedirectInfo(string(raw))
				s.dispatcher.TriggerReloadSlots()
				s.redirect(server, plRsp, false)
			} else if bytes.HasPrefix(raw, ASK) {
				_, server := ParseRedirectInfo(string(raw))
				s.redirect(server, plRsp, true)
			}
		}
	}

	if plRsp.err != nil {
		return plRsp.err
	}

	if !s.closed {
		if err := s.writeResp(plRsp); err != nil {
			return err
		}
	}

	return nil
}

// handleRespPipeline handles the response if its sequence number is equal to session's
// response sequence number, otherwise, put it to a heap to keep the response order is same
// to request order
func (s *Session) handleRespPipeline(plRsp *PipelineResponse) error {
	if plRsp.ctx.seq != s.rspSeq {
		heap.Push(s.rspHeap, plRsp)
		return nil
	}

	if err := s.handleResp(plRsp); err != nil {
		return err
	}
	// continue to check the heap
	for {
		if rsp := s.rspHeap.Top(); rsp == nil || rsp.ctx.seq != s.rspSeq {
			return nil
		}
		rsp := heap.Pop(s.rspHeap).(*PipelineResponse)
		if err := s.handleResp(rsp); err != nil {
			return err
		}
	}
}

func (s *Session) handleMultiCmd(cmd *resp.Command) {
	if cmd.Name() == "MULTI" {
		if s.multiCmd != nil {
			s.handleErrorCmd([]byte("ERR MULTI calls can not be nested"))
		} else {
			s.multiCmd = &[]*resp.Command{}
			s.handleSimpleStringCmd(OK)
		}
	} else if cmd.Name() == "EXEC" {
		if s.multiCmd == nil {
			s.handleErrorCmd([]byte("ERR EXEC without MULTI"))
		} else if s.multiCmdErr {
			s.multiCmdErr = false
			s.handleErrorCmd([]byte("EXECABORT Transaction discarded"))
		} else {
			exec := NewMultiCmdExec(s)
			data, err := exec.Exec()
			if err != nil {
				s.handleErrorCmd([]byte(fmt.Sprintf("ERR EXEC error %v", err)))
			} else {
				s.reqWg.Add(1)
				plRsp := &PipelineResponse{
					rsp: resp.NewObjectFromData(data),
					ctx: &PipelineRequest{seq: s.getNextReqSeq(), wg: s.reqWg},
				}
				s.backQ <- plRsp
			}
		}
		s.multiCmd = nil
	} else {
		flag := CmdFlag(cmd)
		if flag == CMD_FLAG_GENERAL || flag == CMD_FLAG_READ {
			*s.multiCmd = append(*s.multiCmd, cmd)
			s.handleSimpleStringCmd([]byte("QUEUED"))
		} else {
			s.multiCmdErr = true
			s.handleErrorCmd([]byte(UNKNOWN_CMD_ERR))
		}
	}
}

func (s *Session) handleErrorCmd(msg []byte) {
	plReq := &PipelineRequest{
		seq: s.getNextReqSeq(),
		wg:  s.reqWg,
	}
	s.reqWg.Add(1)
	rsp := &resp.Data{T: resp.T_Error, String: msg}
	plRsp := &PipelineResponse{
		rsp: resp.NewObjectFromData(rsp),
		ctx: plReq,
	}
	s.backQ <- plRsp
}

func (s *Session) handleReadAll(cmd *resp.Command) {
	seq := s.getNextReqSeq()
	slots := s.dispatcher.slotTable.ServerSlots()
	mc := NewMultiCmd(s, cmd, len(slots))
	for i, slot := range slots {
		subCmd, err := mc.SubCmd(i, len(slots))
		if err != nil {
			panic(err)
		}
		plReq := &PipelineRequest{
			cmd:       subCmd,
			readOnly:  true,
			slot:      slot,
			seq:       seq,
			subSeq:    i,
			backQ:     s.backQ,
			parentCmd: mc,
			wg:        s.reqWg,
		}
		s.reqWg.Add(1)
		s.Schedule(plReq)
	}
}

func (s *Session) handleAuthCmd(cmd *resp.Command) {
	if len(cmd.Args) == 2 {
		if s.valkeyConn.Auth(cmd.Args[1]) {
			s.handleSimpleStringCmd(OK)
			s.auth = true
		} else {
			s.handleErrorCmd(AUTH_CMD_ERR)
		}
	} else {
		s.handleErrorCmd(ARGUMENTS_ERR)
	}
}

func (s *Session) handleSimpleStringCmd(msg []byte) {
	s.reqWg.Add(1)
	plRsp := &PipelineResponse{
		rsp: resp.NewObjectFromData(&resp.Data{T: resp.T_SimpleString, String: msg}),
		ctx: &PipelineRequest{
			seq: s.getNextReqSeq(),
			wg:  s.reqWg,
		},
	}
	s.backQ <- plRsp
}

func (s *Session) handleGeneralCmd(cmd *resp.Command) {
	key := cmd.Value(1)
	slot := Key2Slot(key)
	plReq := &PipelineRequest{
		cmd:      cmd,
		readOnly: CmdReadOnly(cmd),
		slot:     slot,
		seq:      s.getNextReqSeq(),
		backQ:    s.backQ,
		wg:       s.reqWg,
	}

	s.reqWg.Add(1)
	s.Schedule(plReq)
}

func (s *Session) handleMultiKeyCmd(cmd *resp.Command, numKeys int) {
	mc := NewMultiCmd(s, cmd, numKeys)
	// multi sub cmd share the same seq number
	seq := s.getNextReqSeq()
	for i := 0; i < numKeys; i++ {
		subCmd, err := mc.SubCmd(i, numKeys)
		if err != nil {
			panic(err)
		}
		key := subCmd.Value(1)
		slot := Key2Slot(key)
		plReq := &PipelineRequest{
			cmd:       subCmd,
			readOnly:  CmdReadOnly(cmd),
			slot:      slot,
			seq:       seq,
			subSeq:    i,
			backQ:     s.backQ,
			parentCmd: mc,
			wg:        s.reqWg,
		}
		s.reqWg.Add(1)
		s.Schedule(plReq)
	}
}

func (s *Session) Schedule(req *PipelineRequest) {
	var server string
	if req.readOnly {
		server = s.dispatcher.slotTable.ReadServer(req.slot)
	} else {
		server = s.dispatcher.slotTable.WriteServer(req.slot)
	}

	backendServer, err := s.dispatcher.backendServerPool.Get(server)
	if err != nil {
		s.handleErrorCmd([]byte(fmt.Sprintf("ERR %v", err)))
	} else {
		defer s.dispatcher.backendServerPool.Put(backendServer)
		resp, err := backendServer.Request(req)
		if err == nil {
			s.backQ <- resp
		} else {
			s.handleErrorCmd([]byte(fmt.Sprintf("ERR %v", err)))
		}
	}
	glog.Infof("request count: %d, response count: %d", s.reqSeq, s.rspSeq)
}

func (s *Session) Close() {
	glog.Infof("close session %p", s)
	if !s.closed {
		s.closed = true
		s.Conn.Close()
	}
}

func (s *Session) Read(p []byte) (int, error) {
	return s.r.Read(p)
}

func (s *Session) getNextReqSeq() (seq int64) {
	seq = s.reqSeq
	s.reqSeq++
	return
}

// ParseRedirectInfo parse slot redirect information from MOVED and ASK Error
func ParseRedirectInfo(msg string) (slot int, server string) {
	var err error
	parts := strings.Fields(msg)
	if len(parts) != 3 {
		glog.Fatalf("invalid redirect message: %s", msg)
	}
	slot, err = strconv.Atoi(parts[1])
	if err != nil {
		glog.Fatalf("invalid redirect message: %s", msg)
	}
	server = parts[2]
	return
}
