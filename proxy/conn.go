package proxy

import (
	"bufio"
	"fmt"
	"net"
	"time"

	"github.com/drycc-addons/valkey-cluster-proxy/fnet"
	"github.com/drycc-addons/valkey-cluster-proxy/proto"
	"github.com/golang/glog"
)

type ValkeyConn struct {
	initCap      int
	maxIdle      int
	connTimeout  time.Duration
	password     string
	sendReadOnly bool
}

func NewValkeyConn(initCap, maxIdle int, connTimeout time.Duration, password string, sendReadOnly bool) *ValkeyConn {
	p := &ValkeyConn{
		initCap:      initCap,
		maxIdle:      maxIdle,
		password:     password,
		connTimeout:  connTimeout,
		sendReadOnly: sendReadOnly,
	}
	return p
}

func (cp *ValkeyConn) Conn(server string) (net.Conn, error) {
	dialer := net.Dialer{
		Timeout: cp.connTimeout,
		Control: fnet.ApplySocketOptions(&fnet.ListenConfig{
			SocketReusePort:   true,
			SocketFastOpen:    true,
			SocketDeferAccept: true,
		}),
	}
	conn, err := dialer.Dial("tcp", server)
	if err != nil {
		return nil, err
	}
	return cp.postConnect(conn)
}

func (cp *ValkeyConn) Auth(password string) bool {
	return cp.password == password
}

func (cp *ValkeyConn) postConnect(conn net.Conn) (net.Conn, error) {
	if cp.password != "" {
		cmd, _ := proto.NewCommand("AUTH", cp.password)
		if _, err := cp.Request(cmd, conn); err != nil {
			defer conn.Close()
			return nil, err
		}
	}

	if _, err := cp.Request(VALKEY_CMD_READ_ONLY, conn); err != nil {
		defer conn.Close()
		return nil, err
	}
	return conn, nil
}

func (cp *ValkeyConn) Request(command *proto.Command, conn net.Conn) (*proto.Data, error) {
	if _, err := conn.Write(command.Format()); err != nil {
		glog.Errorf("write %s failed, addr: %s, error: %s", command.Name(), conn.RemoteAddr().String(), err)
		return nil, err
	}

	var data *proto.Data
	reader := bufio.NewReader(conn)
	data, err := proto.ReadData(reader)
	if err != nil {
		glog.Errorf("read %s resp failed, addr: %s, error: %s", command.Name(), conn.RemoteAddr().String(), err)
		return nil, err
	}

	if data.T == proto.T_Error {
		glog.Errorf("%s resp is not OK, addr: %s, msg: %s", command.Name(), conn.RemoteAddr().String(), data.String)
		return nil, fmt.Errorf("post connect error: %s resp is not OK", command.Name())
	}
	return data, nil
}
