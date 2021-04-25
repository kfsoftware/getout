package registry

import (
	"github.com/hashicorp/yamux"
	"io"
	"sync"
	"time"
)

func testConf() *yamux.Config {
	conf := yamux.DefaultConfig()
	conf.AcceptBacklog = 64
	conf.KeepAliveInterval = 100 * time.Millisecond
	conf.ConnectionWriteTimeout = 250 * time.Millisecond
	return conf
}

func testConfNoKeepAlive() *yamux.Config {
	conf := testConf()
	conf.EnableKeepAlive = false
	return conf
}

type pipeConn struct {
	reader       *io.PipeReader
	writer       *io.PipeWriter
	writeBlocker sync.Mutex
}

func (p *pipeConn) Read(b []byte) (int, error) {
	return p.reader.Read(b)
}

func (p *pipeConn) Write(b []byte) (int, error) {
	p.writeBlocker.Lock()
	defer p.writeBlocker.Unlock()
	return p.writer.Write(b)
}

func (p *pipeConn) Close() error {
	p.reader.Close()
	return p.writer.Close()
}
func testConn() (io.ReadWriteCloser, io.ReadWriteCloser) {
	read1, write1 := io.Pipe()
	read2, write2 := io.Pipe()
	conn1 := &pipeConn{reader: read1, writer: write2}
	conn2 := &pipeConn{reader: read2, writer: write1}
	return conn1, conn2
}

func testClientServerConfig(conf *yamux.Config) (*yamux.Session, *yamux.Session) {
	conn1, conn2 := testConn()
	client, _ := yamux.Client(conn1, conf)
	server, _ := yamux.Server(conn2, conf)
	return client, server
}

func testClientServer() (*yamux.Session, *yamux.Session) {
	return testClientServerConfig(testConf())
}
//func TestName(t *testing.T) {
//	client, server := testClientServer()
//	defer client.Close()
//	defer server.Close()
//
//	rtt, err := client.Ping()
//	if err != nil {
//		t.Fatalf("err: %v", err)
//	}
//	if rtt == 0 {
//		t.Fatalf("bad: %v", rtt)
//	}
//
//	rtt, err = server.Ping()
//	if err != nil {
//		t.Fatalf("err: %v", err)
//	}
//	if rtt == 0 {
//		t.Fatalf("bad: %v", rtt)
//	}
//	wg := &sync.WaitGroup{}
//	wg.Add(2)
//	tunnelReg := TunnelRegistry{}
//	go func() {
//		defer wg.Done()
//		_, err = tunnelReg.StoreSession(server)
//		if err != nil {
//			t.Fatalf("err: %v", err)
//		}
//	}()
//	tunnelReq := &messages.TunnelRequest{
//		Req: &messages.TunnelRequest_Http{
//			Http: &messages.HttpTunnelRequest{
//				Host: "a.localhost",
//			},
//		},
//	}
//	conn, err := client.Open()
//	if err != nil {
//		t.Fatalf("err: %v", err)
//	}
//	defer conn.Close()
//	b, err := proto.Marshal(tunnelReq)
//	if err != nil {
//		t.Fatalf("err: %v", err)
//	}
//	err = binary.Write(conn, binary.LittleEndian, int64(len(b)))
//	if err != nil {
//		panic(err)
//	}
//	if _, err = conn.Write(b); err != nil {
//		t.Fatalf("err: %v", err)
//	}
//	wg.Done()
//	wg.Wait()
//}
