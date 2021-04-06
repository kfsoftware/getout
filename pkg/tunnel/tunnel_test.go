package tunnel

import (
	"fmt"
	"github.com/hashicorp/yamux"
	"github.com/kfsoftware/getout/pkg/registry"
	log "github.com/schollz/logger"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
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

func TestName(t *testing.T) {
	client, server := testClientServer()
	defer client.Close()
	defer server.Close()
	tunnelRegistry := registry.NewTunnelRegistry()
	i := &instance{registry: tunnelRegistry}
	testServer, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	log.Infof("Address=%s", testServer.Addr().String())
	incList, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	go i.startMainServer(incList)
	body := "Go is a general-purpose language designed with systems programming in mind."
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Wed, 19 Jul 1972 19:00:00 GMT")
		fmt.Fprintln(w, body)
	}))
	defer ts.Close()
	log.Infof("Address %s", ts.URL)
	tunnelList, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	go i.startTunnelServer(tunnelList)
	wg := &sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		err = tunnelRegistry.StoreSession(server)
		if err != nil {
			t.Fatalf("Err: %v", err)
		}
	}()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	tunnelCli := tunnelClient{sess: client, address: u.Host}
	err = tunnelCli.startHttpTunnel("a.localhost")
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	go func() {
		wg.Done()
		tunnelCli.startListenServer()
	}()
	wg.Wait()
	mainServerAddr := incList.Addr().String()
	chunks := strings.Split(mainServerAddr, ":")
	port := chunks[len(chunks)-1]
	resp, err := http.Get(fmt.Sprintf("http://%s:%s", "localhost", port))
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	bodyStr := string(bodyBytes)
	if !strings.EqualFold(bodyStr, fmt.Sprintf("%s\n", body)) {
		t.Fatalf("Unexpected response, got=%s expected=%s", bodyStr, body)
	}
}
