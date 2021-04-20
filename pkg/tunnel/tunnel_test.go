package tunnel

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/hashicorp/yamux"
	"github.com/kfsoftware/getout/pkg/db"
	"github.com/kfsoftware/getout/pkg/registry"
	log "github.com/schollz/logger"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"io"
	"io/ioutil"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
func publicKey(priv interface{}) interface{} {
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		return &k.PublicKey
	case *ecdsa.PrivateKey:
		return &k.PublicKey
	default:
		return nil
	}
}

func pemBlockForKey(priv interface{}) *pem.Block {
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		return &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}
	case *ecdsa.PrivateKey:
		b, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Unable to marshal ECDSA private key: %v", err)
			os.Exit(2)
		}
		return &pem.Block{Type: "EC PRIVATE KEY", Bytes: b}
	default:
		return nil
	}
}

type ServerConfig struct {
	Address string
	Client  *http.Client
}

func startTlsServer(handler http.Handler, sni string) (*http.Server, *ServerConfig, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Acme Co"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour * 24 * 180),
		DNSNames:              []string{sni},
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	/*
	   hosts := strings.Split(*host, ",")
	   for _, h := range hosts {
	   	if ip := net.ParseIP(h); ip != nil {
	   		template.IPAddresses = append(template.IPAddresses, ip)
	   	} else {
	   		template.DNSNames = append(template.DNSNames, h)
	   	}
	   }
	   if *isCA {
	   	template.IsCA = true
	   	template.KeyUsage |= x509.KeyUsageCertSign
	   }
	*/

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, publicKey(priv), priv)
	if err != nil {
		return nil, nil, err
	}
	crtBuffer := &bytes.Buffer{}
	err = pem.Encode(crtBuffer, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	if err != nil {
		return nil, nil, err
	}
	crtBytes := crtBuffer.Bytes()
	pkBuffer := &bytes.Buffer{}
	err = pem.Encode(pkBuffer, pemBlockForKey(priv))
	pkBytes := pkBuffer.Bytes()
	pair, err := tls.X509KeyPair(crtBytes, pkBytes)
	if err != nil {
		return nil, nil, err
	}
	config := &tls.Config{
		Certificates: []tls.Certificate{pair},
		NextProtos:   []string{"http/1.1"},
	}
	s := &http.Server{
		Handler:   handler,
		TLSConfig: config,
	}
	certpool := x509.NewCertPool()
	certificate, err := x509.ParseCertificate(derBytes)
	if err != nil {
		return nil, nil, err
	}
	certpool.AddCert(certificate)
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: certpool,
			},
			ForceAttemptHTTP2: false,
		},
	}
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, nil, err
	}
	list := tls.NewListener(ln, config)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err = s.Serve(list)
		if err != nil {
			log.Errorf("error=%v", err)
		}
	}()
	address := ln.Addr().String()
	return s, &ServerConfig{Address: address, Client: client}, nil
}

func getDb() *gorm.DB {
	dbClient, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		panic(err)
	}
	err = dbClient.AutoMigrate(&db.Tunnel{})
	if err != nil {
		panic(err)
	}
	return dbClient
}

func TestTls(t *testing.T) {
	client, server := testClientServer()
	defer func(client *yamux.Session) {
		_ = client.Close()
	}(client)
	defer func(server *yamux.Session) {
		_ = server.Close()
	}(server)
	db := getDb()
	tunnelRegistry := registry.NewTunnelRegistry(db)
	i := &instance{registry: tunnelRegistry}
	testServer, err := net.Listen("tcp", "127.0.0.1:0")
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
	handlerFunc := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", "Wed, 19 Jul 1972 19:00:00 GMT")
		fmt.Fprintln(w, body)
	})
	ts, cfg, err := startTlsServer(handlerFunc, "localhost")
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	defer ts.Close()
	log.Infof("Address %s", cfg.Address)
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
	tunnelCli := tunnelClient{sess: client, address: cfg.Address}
	err = tunnelCli.startTlsTunnel("localhost")
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
	resp, err := cfg.Client.Get(fmt.Sprintf("https://%s:%s", "localhost", port))
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

func TestHttp(t *testing.T) {
	client, server := testClientServer()
	defer client.Close()
	defer server.Close()
	db := getDb()
	tunnelRegistry := registry.NewTunnelRegistry(db)
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
	err = tunnelCli.startHttpTunnel("localhost")
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
	r := strings.NewReader(`{"foo":"bar"}`)
	resp, err := http.Post(fmt.Sprintf("http://%s:%s", "localhost", port), "application/json", r)
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

func TestHttps(t *testing.T) {
	client, server := testClientServer()
	defer client.Close()
	defer server.Close()
	clientDb := getDb()
	tunnelRegistry := registry.NewTunnelRegistry(clientDb)
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
	err = tunnelCli.startHttpTunnel("localhost")
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
	//time.Sleep(100 * time.Second)
	m := map[string]string{}
	for i := 0; i < 1000; i++ {
		m[fmt.Sprintf("key%d", i)] = fmt.Sprintf("really long string ......................... %d", i)
	}
	jsonBytes, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	r := bytes.NewReader(jsonBytes)
	resp, err := http.Post(fmt.Sprintf("https://%s:%s", "localhost", port), "application/json", r)
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

func benchmarkHttps(n int, b *testing.B) {
	client, server := testClientServer()
	defer client.Close()
	defer server.Close()
	clientDb := getDb()
	tunnelRegistry := registry.NewTunnelRegistry(clientDb)
	i := &instance{registry: tunnelRegistry}
	testServer, err := net.Listen("tcp", ":0")
	if err != nil {
		b.Fatalf("Err: %v", err)
	}
	log.Infof("Address=%s", testServer.Addr().String())
	incList, err := net.Listen("tcp", ":0")
	if err != nil {
		b.Fatalf("Err: %v", err)
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
		b.Fatalf("Err: %v", err)
	}
	go i.startTunnelServer(tunnelList)
	wg := &sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		err = tunnelRegistry.StoreSession(server)
		if err != nil {
			b.Fatalf("Err: %v", err)
		}
	}()
	u, err := url.Parse(ts.URL)
	if err != nil {
		b.Fatalf("Err: %v", err)
	}
	tunnelCli := tunnelClient{sess: client, address: u.Host}
	err = tunnelCli.startHttpTunnel("localhost")
	if err != nil {
		b.Fatalf("Err: %v", err)
	}
	go func() {
		wg.Done()
		tunnelCli.startListenServer()
	}()
	wg.Wait()
	mainServerAddr := incList.Addr().String()
	chunks := strings.Split(mainServerAddr, ":")
	port := chunks[len(chunks)-1]
	m := map[string]string{}
	for i := 0; i < 1000; i++ {
		m[fmt.Sprintf("key%d", i)] = fmt.Sprintf("really long string ......................... %d", i)
	}
	jsonBytes, err := json.Marshal(m)
	if err != nil {
		b.Fatalf("Err: %v", err)
	}

	for i := 0; i < n; i++ {
		r := bytes.NewReader(jsonBytes)
		resp, err := http.Post(fmt.Sprintf("https://%s:%s", "localhost", port), "application/json", r)
		if err != nil {
			b.Fatalf("Err: %v", err)
		}
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			b.Fatalf("Err: %v", err)
		}
		err = resp.Body.Close()
		if err != nil {
			b.Fatalf("Err: %v", err)
		}
		bodyStr := string(bodyBytes)
		if !strings.EqualFold(bodyStr, fmt.Sprintf("%s\n", body)) {
			b.Fatalf("Unexpected response, got=%s expected=%s", bodyStr, body)
		}
	}

}

func BenchmarkTest(b *testing.B) {
	log.Debug(fmt.Sprintf("Benchmarking %d", b.N))
	for i := 0; i < b.N; i++ {
		time.Sleep(1 * time.Second)
	}
}

func BenchmarkHttps40(b *testing.B) {
	print(fmt.Sprintf("Benchmarking %d", b.N))
	benchmarkHttps(b.N, b)
}
