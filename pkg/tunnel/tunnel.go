package tunnel

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"github.com/golang/protobuf/proto"
	"github.com/hashicorp/yamux"
	"github.com/kfsoftware/getout/pkg/messages"
	"github.com/kfsoftware/getout/pkg/registry"
	"github.com/pires/go-proxyproto"
	"github.com/pkg/errors"
	log "github.com/schollz/logger"
	"io"
	"net"
	"net/http"
	"net/textproto"
)

type tunnelClient struct {
	sess    *yamux.Session
	address string
}

func (c *tunnelClient) startHttpTunnel(host string) error {
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
		panic(err)
	}
	if _, err = conn.Write(b); err != nil {
		return err
	}
	return nil
}

func (c *tunnelClient) startTlsTunnel(sni string) error {
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
		panic(err)
	}
	if _, err = conn.Write(b); err != nil {
		return err
	}
	return nil
}

func (c *tunnelClient) startListenServer() error {
	for {
		conn, err := c.sess.Accept()
		if err != nil {
			log.Tracef("Failed to accept connections: %v", err)
			return err
		}
		destConn, err := net.Dial("tcp", c.address)
		if err != nil {
			panic(err)
		}
		log.Debugf("client %s connected", conn.RemoteAddr().String())
		copyConn := func(writer, reader net.Conn) {
			defer writer.Close()
			defer reader.Close()
			_, err := io.Copy(writer, reader)
			if err != nil {
				fmt.Printf("io.Copy error: %s", err)
			}
			log.Infof("Connection finished")
		}
		go copyConn(conn, destConn)
		go copyConn(destConn, conn)
	}
}

type instance struct {
	registry *registry.TunnelRegistry
}

// WriteCloser describes a net.Conn with a CloseWrite method.
type WriteCloser interface {
	net.Conn
	// CloseWrite on a network connection, indicates that the issuer of the call
	// has terminated sending on that connection.
	// It corresponds to sending a FIN packet.
	CloseWrite() error
}

func doProxyTcp(conn net.Conn, sess *yamux.Session) {
	destConn, err := sess.Open()
	if err != nil {
		log.Warnf("Connection closed")
		conn.Close()
		return
	}
	copyConn := func(writer net.Conn, reader net.Conn) {
		defer writer.Close()
		defer reader.Close()
		_, err := io.Copy(writer, reader)
		if err != nil {
			log.Tracef("io.Copy error: %s", err)
		}
		log.Infof("Connection finished")
	}
	copyStream := func(writer net.Conn, reader io.Reader) {
		defer writer.Close()
		_, err := io.Copy(writer, reader)
		if err != nil {
			log.Tracef("io.Copy error: %s", err)
		}
		log.Infof("Connection finished")
	}
	go copyStream(destConn, conn)
	go copyConn(conn, destConn)
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
func writeCloser(conn net.Conn) (WriteCloser, error) {
	switch typedConn := conn.(type) {
	case *proxyproto.Conn:
		underlying, ok := typedConn.TCPConn()
		if !ok {
			return nil, fmt.Errorf("underlying connection is not a tcp connection")
		}
		return &writeCloserWrapper{writeCloser: underlying, Conn: typedConn}, nil
	case *net.TCPConn:
		return typedConn, nil
	case *tls.Conn:
		return typedConn, nil
	default:
		return nil, fmt.Errorf("unknown connection type %T", typedConn)
	}
}

func (t *instance) startMainServer(server net.Listener) {
	for {
		conn, err := server.Accept()
		if err != nil {
			panic(err)
		}
		log.Debugf("client %s connected", conn.RemoteAddr().String())
		wc, err := writeCloser(conn)
		if err != nil {
			panic(err)
		}
		br := bufio.NewReader(wc)
		clientInfo, isTls, peeked, err := peekClientHello(br)
		c := &Conn{Peeked: peeked, WriteCloser: wc}
		var sess *yamux.Session
		log.Debugf("Original isTls=%v clientInfo=%v", isTls, clientInfo)
		if isTls {
			log.Debugf("Connection TLS")
			sess, err = t.registry.GetTLSSession(clientInfo)
			if err != nil {
				// If there's not a session
				// maybe there's because there's an HTTPS session
				// we can't close the request at this point
			} else {
				doProxyTcp(c, sess)
				continue
			}
		} else if !isTls {
			br := bufio.NewReader(c)
			headers, err := readHttpRequest(br)
			if err != nil {
				conn.Close()
				log.Warnf("Connection closed")
				continue
			}
			c = &Conn{Peeked: peeked, WriteCloser: wc}
			log.Warnf("Connection not TLS")
			sess, err = t.registry.GetHttpSession(headers)
			if err != nil {
				conn.Close()
				log.Warnf("Connection closed")
				continue
			} else {
				doProxyTcp(c, sess)
				continue
			}
		}
		if sess == nil && isTls {
			// it's https
			crt, err := tls.LoadX509KeyPair("/disco-grande/go/src/github.com/kfsoftware/getout/localhost.pem", "/disco-grande/go/src/github.com/kfsoftware/getout/localhost-key.pem")
			if err != nil {
				conn.Close()
				log.Warnf("Connection closed")
				continue
			}
			server := tls.Server(c, &tls.Config{
				Certificates: []tls.Certificate{crt},
			})
			err = server.Handshake()
			if err != nil {
				conn.Close()
				log.Errorf("Connection closed %v", err)
				continue
			}
			wcTls, err := writeCloser(server)
			if err != nil {
				panic(err)
			}
			br := bufio.NewReader(wcTls)
			peekedBytes := new(bytes.Buffer)
			teeReader := io.TeeReader(br, peekedBytes)
			header, err := readHttpRequest(teeReader)
			if err != nil {
				conn.Close()
				log.Errorf("Error reading headers=%v", err)
				continue
			}
			sess, err := t.registry.GetHttpSession(header)
			if err != nil {
				conn.Close()
				log.Errorf("Connection closed %v", err)
				continue
			}
			c := &Conn{Peeked: peekedBytes.Bytes(), WriteCloser: wcTls}
			doProxyTcp(c, sess)
		}
	}
}

func (t *instance) startTunnelServer(muxServer net.Listener) {
	for {
		conn, err := muxServer.Accept()
		if err != nil {
			log.Warnf("Couldn't accept the connection: %v", err)
			continue
		}
		log.Debugf("client %s connected", conn.RemoteAddr().String())
		sess, err := yamux.Server(conn, nil)
		if err != nil {
			log.Warnf("Couldn't setup yamux server: %v", err)
			continue
		}
		err = t.registry.StoreSession(
			sess,
		)
		if err != nil {
			log.Warnf("Couldn't store session: %v", err)
			continue
		}
	}
}

func readHttpRequest(r io.Reader) (http.Header, error) {
	br := bufio.NewReader(r)
	tp := textproto.NewReader(br)
	var s string
	var err error
	if s, err = tp.ReadLine(); err != nil {
		return nil, err
	}
	log.Infof("First line %s", s)
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
