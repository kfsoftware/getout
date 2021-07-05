package tunnel

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/golang/protobuf/proto"

	"github.com/hashicorp/yamux"
	"github.com/kfsoftware/getout/pkg/messages"
	"github.com/kfsoftware/getout/pkg/registry"
	"github.com/pkg/errors"
	log "github.com/schollz/logger"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"sync"
	"time"
)

type tunnelClient struct {
	sess    *yamux.Session
	address string
}

func NewTunnelClient(
	sess *yamux.Session,
	localAddress string,
) *tunnelClient {
	return &tunnelClient{
		sess:    sess,
		address: localAddress,
	}
}
func (c *tunnelClient) StartHttpTunnel(host string) error {
	tunnelReq := &messages.TunnelRequest{
		Req: &messages.TunnelRequest_Http{
			Http: &messages.HttpTunnelRequest{
				Host: host,
			},
		},
	}
	conn, err := c.sess.Open()
	if err != nil {
		return err
	}
	defer conn.Close()
	b, err := proto.Marshal(tunnelReq)
	if err != nil {
		return err
	}
	err = binary.Write(conn, binary.LittleEndian, int64(len(b)))
	if err != nil {
		return err
	}
	if _, err = conn.Write(b); err != nil {
		return err
	}
	return nil
}

func (c *tunnelClient) StartTlsTunnel(sni string) error {
	tunnelReq := &messages.TunnelRequest{
		Req: &messages.TunnelRequest_Tls{
			Tls: &messages.TlsTunnelRequest{
				Sni: sni,
			},
		},
	}
	conn, err := c.sess.Open()
	if err != nil {
		return err
	}
	defer conn.Close()
	b, err := proto.Marshal(tunnelReq)
	if err != nil {
		return err
	}
	err = binary.Write(conn, binary.LittleEndian, int64(len(b)))
	if err != nil {
		return err
	}
	if _, err = conn.Write(b); err != nil {
		return err
	}
	return nil
}

func (c *tunnelClient) Start() error {
	for {
		conn, err := c.sess.Accept()
		if err != nil {
			log.Warnf("Failed to accept connections: %v", err)
			return err
		}
		destConn, err := net.Dial("tcp", c.address)
		if err != nil {
			log.Warnf("Failed to invoke %s, closing connection: %v", c.address, err)
			conn.Close()
			continue
		}
		log.Debugf("client %s connected", conn.RemoteAddr().String())
		copyConn := func(writer, reader net.Conn) {
			defer writer.Close()
			defer reader.Close()
			_, err := io.Copy(writer, reader)
			if err != nil {
				fmt.Printf("io.Copy error: %s", err)
			}
		}
		go copyConn(conn, destConn)
		go copyConn(destConn, conn)
	}
}
func (c *tunnelClient) StartHttps() error {
	for {
		conn, err := c.sess.Accept()
		if err != nil {
			log.Warnf("Failed to accept connections: %v", err)
			return err
		}
		destConn, err := tls.Dial("tcp", c.address, &tls.Config{
			VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
				return nil
			},
			VerifyConnection: func(state tls.ConnectionState) error {
				return nil
			},
			InsecureSkipVerify: true,
		})
		if err != nil {
			return err
		}
		log.Debugf("client %s connected", conn.RemoteAddr().String())
		copyConn := func(writer, reader net.Conn) {
			defer writer.Close()
			defer reader.Close()
			_, err := io.Copy(writer, reader)
			if err != nil {
				fmt.Printf("io.Copy error: %s", err)
			}
		}
		go copyConn(conn, destConn)
		go copyConn(destConn, conn)
	}
}

func NewTunnelServerInstance(
	registry registry.TunnelRegistry,
	tunnelListener net.Listener,
	serverListener net.Listener,
	adminListener net.Listener,
	defaultDomain string,
	certificates []tls.Certificate,
) *instance {
	return &instance{
		registry:       registry,
		serverListener: serverListener,
		tunnelListener: tunnelListener,
		adminListener:  adminListener,
		defaultDomain:  defaultDomain,
		certificates:   certificates,
	}
}

type instance struct {
	registry       registry.TunnelRegistry
	certificates   []tls.Certificate
	defaultDomain  string
	serverListener net.Listener
	tunnelListener net.Listener
	adminListener  net.Listener
}

// WriteCloser describes a net.Conn with a CloseWrite method.
type WriteCloser interface {
	net.Conn
	// CloseWrite on a network connection, indicates that the issuer of the call
	// has terminated sending on that connection.
	// It corresponds to sending a FIN packet.
	CloseWrite() error
}

func doProxyTcp(conn WriteCloser, sess *yamux.Session) {
	destConn, err := sess.Open()
	if err != nil {
		log.Warnf("Connection closed proxy tcp %s %s", conn.LocalAddr().String(), conn.RemoteAddr().String())
		conn.Close()
		return
	}
	dcw, err := writeCloser(destConn)
	if err != nil {
		log.Warnf("Connection closed proxy tcp %s %s", conn.LocalAddr().String(), conn.RemoteAddr().String())
		destConn.Close()
		return
	}
	copyConn := func(writer, reader WriteCloser) {
		_, err := io.Copy(writer, reader)
		if err != nil {
			log.Debugf("io.Copy error: %s", err)
		}
		errClose := writer.CloseWrite()
		if errClose != nil {
			log.Debugf("Error while terminating connection: %v", errClose)
			return
		}
		log.Infof("Connection finished")
	}
	go copyConn(dcw, conn)
	go copyConn(conn, dcw)
}

type Conn struct {
	// Peeked are the bytes that have been read from Conn for the
	// purposes of route matching, but have not yet been consumed
	// by Read calls. It set to nil by Read when fully consumed.
	Peeked []byte

	// Conn is the underlying connection.
	// It can be type asserted against *net.TCPConn or other types
	// as needed. It should not be read from directly unless
	// Peeked is nil.
	WriteCloser
}

// Read reads bytes from the connection (using the buffer prior to actually reading).
func (c *Conn) Read(p []byte) (n int, err error) {
	if len(c.Peeked) > 0 {
		n = copy(p, c.Peeked)
		c.Peeked = c.Peeked[n:]
		if len(c.Peeked) == 0 {
			c.Peeked = nil
		}
		return n, nil
	}
	return c.WriteCloser.Read(p)
}

type writeCloserWrapper struct {
	net.Conn
	writeCloser WriteCloser
}

func (c *writeCloserWrapper) CloseWrite() error {
	return c.writeCloser.CloseWrite()
}

type writeCloserYamux struct {
	*yamux.Stream
}

func (c *writeCloserYamux) CloseWrite() error {
	return c.Stream.Close()
}
func writeCloser(conn net.Conn) (WriteCloser, error) {
	switch typedConn := conn.(type) {
	case *yamux.Stream:
		return &writeCloserYamux{Stream: typedConn}, nil
	case *net.TCPConn:
		return typedConn, nil
	case *tls.Conn:
		return typedConn, nil
	default:
		return nil, fmt.Errorf("unknown connection type %T", typedConn)
	}
}

const (
	NotFound = `HTTP/1.1 404 Not Found
Content-Length: %d

Tunnel %s not found
`
)

func (t *instance) tunnelHttpsNotFound(conn net.Conn) error {
	return conn.Close()
}
func (t *instance) tunnelHttpNotFound(headers http.Header, conn net.Conn) error {
	host := headers.Get("Host")
	_, err := conn.Write([]byte(fmt.Sprintf(NotFound, len(host)+18, host)))
	return err
}
func (t *instance) startAdminServer(listener net.Listener) error {
	r := gin.Default()
	gin.DebugPrintRouteFunc = func(httpMethod, absolutePath, handlerName string, nuHandlers int) {
		log.Debugf("endpoint %v %v %v %v\n", httpMethod, absolutePath, handlerName, nuHandlers)
	}

	r.GET("/tunnels", func(c *gin.Context) {
		httpTunnels, err := t.registry.GetHttpTunnels()
		if err != nil {
			c.JSON(400, map[string]string{
				"Message": err.Error(),
			})
			return
		}
		tlsTunnels, err := t.registry.GetTLSTunnels()
		if err != nil {
			c.JSON(400, map[string]string{
				"Message": err.Error(),
			})
			return
		}
		var toReturnHttpsTunnels []map[string]interface{}
		for _, httpsTunnel := range httpTunnels {
			elems := map[string]interface{}{
				"id": httpsTunnel.GetID(),
			}
			toReturnHttpsTunnels = append(toReturnHttpsTunnels, elems)
			props, err := httpsTunnel.GetProperties()
			if err == nil {
				elems["sni"] = props.Host
				elems["raddr"] = props.RemoteAddress
			}
		}
		var toReturnTlsTunnels []map[string]interface{}
		for _, tlsTunnel := range tlsTunnels {

			elems := map[string]interface{}{
				"id": tlsTunnel.GetID(),
			}
			props, err := tlsTunnel.GetProperties()
			if err == nil {
				elems["sni"] = props.SNI
				elems["raddr"] = props.RemoteAddress
			}
			toReturnTlsTunnels = append(toReturnTlsTunnels, elems)

		}

		obj := map[string]interface{}{
			"https": toReturnHttpsTunnels,
			"tls":   toReturnTlsTunnels,
		}

		c.JSON(http.StatusOK, obj)
	})
	return r.RunListener(listener)
}
func (t *instance) startMainServer(server net.Listener) error {
	log.Infof("Listening for requests on %s", server.Addr().String())
	for {
		log.Tracef("Accepting new connections")
		conn, err := server.Accept()
		if err != nil {
			log.Tracef("Failed accepting new connections: %v", err)
			return err
		}
		log.Debugf("client %s connected", conn.RemoteAddr().String())
		wc, err := writeCloser(conn)
		if err != nil {
			log.Tracef("Failed creating a writeCloser: %v", err)
			return err
		}
		br := bufio.NewReader(wc)
		clientInfo, isTls, peeked, err := peekClientHello(br)
		c := &Conn{Peeked: peeked, WriteCloser: wc}
		var sess *yamux.Session
		if isTls {
			log.Debugf("Connection TLS")
			sess, err = t.registry.GetTLSSession(clientInfo)
			if err != nil {
				log.Tracef("No tls session found: %v", err)
				// If there's not a session
				// maybe there's because there's an HTTPS session
				// we can't close the request at this point
			} else {
				doProxyTcp(c, sess)
				continue
			}
		} else if !isTls {
			err := c.Close()
			if err != nil {
				log.Warnf("Error CloseWrite: %v", err)
			}
			err = c.CloseWrite()
			if err != nil {
				log.Warnf("Error CloseWrite: %v", err)
			}
			continue
		}
		if sess == nil && isTls && len(t.certificates) > 0 {
			log.Trace("Session null and connection is TLS, looking for HTTP headers")
			// it's https
			server := tls.Server(c, &tls.Config{
				Certificates: t.certificates,
			})
			err = server.Handshake()
			if err != nil {
				log.Warnf("Connection closed %v", err)
				continue
			}
			log.Tracef("Handhsake performed")
			wcTls, err := writeCloser(server)
			if err != nil {
				return err
			}
			log.Tracef("Write closer created")
			header := http.Header{}
			header["Host"] = []string{clientInfo.ServerName}
			log.Tracef("TLS headers read=%v", header)
			sess, err = t.registry.GetHttpSession(header)
			if err != nil {
				log.Warnf("Closing connection: %v", err)
				err = t.tunnelHttpNotFound(header, wcTls)
				if err != nil {
					log.Warnf("Error sending 404: %v", err)
				}
				err = wcTls.Close()
				if err != nil {
					log.Warnf("Error Close: %v", err)
				}
				err = wcTls.CloseWrite()
				if err != nil {
					log.Warnf("Error CloseWrite: %v", err)
				}
				continue
			}
			doProxyTcp(wcTls, sess)
		} else {
			conn.Close()
		}
	}
}
func (t *instance) Start() error {
	wg := &sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		err := t.startMainServer(
			t.serverListener,
		)
		if err != nil {
			log.Errorf("Failed to start listening server:%v", err)
		}
	}()
	go func() {
		defer wg.Done()
		err := t.startTunnelServer(
			t.tunnelListener,
		)
		if err != nil {
			log.Errorf("Failed to start tunnel server:%v", err)
		}
	}()
	go func() {
		defer wg.Done()
		err := t.startAdminServer(
			t.adminListener,
		)
		if err != nil {
			log.Errorf("Failed to start admin server:%v", err)
		}
	}()
	wg.Wait()
	return nil
}
func (t *instance) startTunnelServer(muxServer net.Listener) error {
	log.Infof("Tunnel server listening on %s", muxServer.Addr().String())
	for {
		conn, err := muxServer.Accept()
		if err != nil {
			log.Warnf("Couldn't accept the connection: %v", err)
			continue
		}
		log.Debugf("client %s connected", conn.RemoteAddr().String())
		cfg := yamux.DefaultConfig()
		sess, err := yamux.Server(conn, cfg)
		if err != nil {
			log.Warnf("Couldn't setup yamux server: %v", err)
			continue
		}
		tunn, err := t.registry.StoreSession(
			sess,
		)
		if err != nil {
			log.Warnf("Couldn't store session: %v", err)
			continue
		}
		go func() {
			for {
				_, err := sess.Ping()
				if err != nil {
					log.Warnf("Removing session %s doesn't seem active: %v", tunn.GetID(), err)
					err = t.registry.RemoveSession(tunn.GetID())
					if err != nil {
						log.Errorf("Failed removing session %s: %v", tunn.GetID(), err)
					}
					break
				}
				time.Sleep(5 * time.Second)
			}
		}()
	}
}

func readHttpRequest(r io.Reader) (http.Header, error) {
	br := bufio.NewReader(r)
	tp := textproto.NewReader(br)
	var err error
	if _, err = tp.ReadLine(); err != nil {
		return nil, err
	}
	mimeHeader, err := tp.ReadMIMEHeader()
	headers := http.Header(mimeHeader)
	if err != nil {
		return nil, err
	}
	return headers, nil
}

func peekClientHello(br *bufio.Reader) (*tls.ClientHelloInfo, bool, []byte, error) {
	hello, isTls, peeked, err := readClientHello(br)
	if err != nil {
		return nil, false, nil, err
	}
	return hello, isTls, peeked, nil
}

func readClientHello(br *bufio.Reader) (*tls.ClientHelloInfo, bool, []byte, error) {
	return clientHelloServerName(br)
}

func clientHelloServerName(br *bufio.Reader) (*tls.ClientHelloInfo, bool, []byte, error) {
	hdr, err := br.Peek(1)
	if err != nil {
		var opErr *net.OpError
		if !errors.Is(err, io.EOF) && (!errors.As(err, &opErr) || opErr.Timeout()) {
			log.Debugf("Error while Peeking first byte: %s", err)
		}

		return nil, false, nil, err
	}

	// No valid TLS record has a type of 0x80, however SSLv2 handshakes
	// start with a uint16 length where the MSB is set and the first record
	// is always < 256 bytes long. Therefore typ == 0x80 strongly suggests
	// an SSLv2 client.
	const recordTypeSSLv2 = 0x80
	const recordTypeHandshake = 0x16
	if hdr[0] != recordTypeHandshake {
		if hdr[0] == recordTypeSSLv2 {
			// we consider SSLv2 as TLS and it will be refuse by real TLS handshake.
			return nil, true, getPeeked(br), nil
		}
		return nil, false, getPeeked(br), nil // Not TLS.
	}

	const recordHeaderLen = 5
	hdr, err = br.Peek(recordHeaderLen)
	if err != nil {
		log.Errorf("Error while Peeking hello: %s", err)
		return nil, false, getPeeked(br), nil
	}

	recLen := int(hdr[3])<<8 | int(hdr[4]) // ignoring version in hdr[1:3]
	helloBytes, err := br.Peek(recordHeaderLen + recLen)
	if err != nil {
		log.Errorf("Error while Hello: %s", err)
		return nil, true, getPeeked(br), nil
	}

	var clientHello *tls.ClientHelloInfo
	server := tls.Server(sniSniffConn{r: bytes.NewReader(helloBytes)}, &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			clientHello = hello
			return nil, nil
		},
	})
	_ = server.Handshake()

	return clientHello, true, getPeeked(br), nil
}

// sniSniffConn is a net.Conn that reads from r, fails on Writes,
// and crashes otherwise.
type sniSniffConn struct {
	r        io.Reader
	net.Conn // nil; crash on any unexpected use
}

// Read reads from the underlying reader.
func (c sniSniffConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Write crashes all the time.
func (sniSniffConn) Write(p []byte) (int, error) { return 0, io.EOF }

func getPeeked(br *bufio.Reader) []byte {
	peeked, err := br.Peek(br.Buffered())
	if err != nil {
		log.Errorf("Could not get anything: %s", err)
		return nil
	}
	return peeked
}
