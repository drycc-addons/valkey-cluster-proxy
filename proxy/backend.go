package proxy

import (
	"bufio"
	"container/list"
	"errors"
	"io"
	"net"
	"time"

	resp "github.com/drycc-addons/valkey-cluster-proxy/proto"
	"github.com/golang/glog"
)

type BackendServer struct {
	inflight   *list.List
	server     string
	conn       net.Conn
	r          *bufio.Reader
	w          *bufio.Writer
	valkeyConn *ValkeyConn
}

func NewBackendServer(server string, valkeyConn *ValkeyConn) *BackendServer {
	tr := &BackendServer{
		inflight:   list.New(),
		server:     server,
		valkeyConn: valkeyConn,
	}

	if conn, err := valkeyConn.Conn(server); err != nil {
		glog.Error(tr.server, err)
	} else {
		tr.initRWConn(conn)
	}

	return tr
}

func (tr *BackendServer) Request(req *PipelineRequest) (*PipelineResponse, error) {
	if err := tr.writeToBackend(req); err != nil {
		glog.Error(err)
		if err := tr.tryRecover(err); err != nil {
			return nil, err
		}
		return nil, err
	}
	rsp := resp.NewObject()

	if err := resp.ReadDataBytes(tr.r, rsp); err != nil {
		glog.Error(err)
		if err := tr.tryRecover(err); err != nil {
			return nil, err
		}
		return nil, err
	}
	plReq := tr.inflight.Remove(tr.inflight.Front()).(*PipelineRequest)
	return &PipelineResponse{ctx: plReq, rsp: rsp}, nil
}

func (tr *BackendServer) writeToBackend(plReq *PipelineRequest) error {
	var err error
	// always put req into inflight list first
	tr.inflight.PushBack(plReq)

	if tr.w == nil {
		err = errors.New("init task runner connection error")
		glog.Error(err)
		return err
	}
	buf := plReq.cmd.Format()
	if _, err = tr.w.Write(buf); err != nil {
		glog.Error(err)
		return err
	}
	err = tr.w.Flush()
	if err != nil {
		glog.Error("flush error", err)
	}
	return err
}

func (tr *BackendServer) tryRecover(err error) error {
	tr.cleanupInflight(err)

	//try to recover
	if conn, err := tr.valkeyConn.Conn(tr.server); err != nil {
		glog.Error("try to recover from error failed", tr.server, err)
		time.Sleep(100 * time.Millisecond)
		return err
	} else {
		glog.Info("recover success", tr.server)
		tr.initRWConn(conn)
	}

	return nil
}

func (tr *BackendServer) cleanupInflight(err error) {
	for e := tr.inflight.Front(); e != nil; {
		plReq := e.Value.(*PipelineRequest)
		if err != io.EOF {
			glog.Error("clean up", plReq)
		}
		plRsp := &PipelineResponse{
			ctx: plReq,
			err: err,
		}
		plReq.backQ <- plRsp
		next := e.Next()
		tr.inflight.Remove(e)
		e = next
	}
}

func (tr *BackendServer) initRWConn(conn net.Conn) {
	if tr.conn != nil {
		tr.conn.Close()
	}
	tr.conn = conn
	tr.r = bufio.NewReaderSize(tr.conn, 1024*512)
	tr.w = bufio.NewWriterSize(tr.conn, 1024*512)
}

func (tr *BackendServer) Close() error {
	if tr.conn != nil {
		return tr.conn.Close()
	}
	return nil
}
