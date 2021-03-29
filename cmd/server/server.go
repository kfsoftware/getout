package server

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"github.com/hashicorp/yamux"
	log "github.com/schollz/logger"
	"github.com/spf13/cobra"
	"io"
	"net"
	"time"
)

type serverCmd struct {
	tunnelAddr string
	addr       string

}

func (c *serverCmd) validate() error {
	return nil
}
func (c *serverCmd) run() error {
	server, err := net.Listen("tcp", c.addr)
	if err != nil {
		panic(fmt.Errorf("error listening on %s: %w", c.addr, err))
	}
	defer server.Close()
	muxServer, err := net.Listen("tcp", c.tunnelAddr)
	if err != nil {
		panic(fmt.Errorf("error listening on %s: %w", c.tunnelAddr, err))
	}
	defer muxServer.Close()
	var sessions []*Session
	go func() {
		for {
			conn, err := muxServer.Accept()
			if err != nil {
				log.Warnf("Connection closed")
				return
			}
			log.Debugf("client %s connected", conn.RemoteAddr().String())
			sess, err := yamux.Server(conn, nil)
			if err != nil {
				panic(err)
			}
			initialConn, err := sess.Accept()
			if err != nil {
				panic(err)
			}
			var sz int64
			err = binary.Read(initialConn, binary.LittleEndian, &sz)
			sni := make([]byte, sz)
			n, err := initialConn.Read(sni)
			log.Debugf("Read message %s %d", sni, n)
			if err != nil {
				panic(err)
			}
			sessions = append(sessions, &Session{
				sni:  string(sni),
				sess: sess,
			})
		}
	}()
	for {
		conn, err := server.Accept()
		if err != nil {
			panic(err)
		}
		log.Debugf("client %s connected", conn.RemoteAddr().String())

		clientHello, originalConn, err := peekClientHello(conn)
		if err != nil {
			log.Errorf("Error extracting client hello %v", err)
		}
		sni := clientHello.ServerName
		log.Infof("SNI=%s", sni)
		if len(sessions) == 0 {
			conn.Close()
			continue
		}
		var destSess *Session
		for _, session := range sessions {
			if session.sni == sni {
				destSess = session
			}
		}
		if destSess == nil {
			log.Warnf("Session not found")
			conn.Close()
			continue
		}
		destConn, err := destSess.sess.Open()
		if err != nil {
			conn.Close()
			sessions = RemoveIndex(sessions, 0)
			log.Warnf("Connection closed")
			continue
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

		go copyStream(destConn, originalConn)
		go copyConn(conn, destConn)
	}
}

func NewServerCmd() *cobra.Command {
	c := &serverCmd{}
	cmd := &cobra.Command{
		Use: "client",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.validate(); err != nil {
				return err
			}
			return c.run()
		},
	}
	persistentFlags := cmd.PersistentFlags()
	persistentFlags.StringVarP(&c.addr, "addr", "", "", "Address to listen for requests")
	persistentFlags.StringVarP(&c.tunnelAddr, "tunnel-addr", "", "", "Address to manage the tunnel connections")

	cmd.MarkPersistentFlagRequired("addr")
	cmd.MarkPersistentFlagRequired("tunnel-addr")
	return cmd
}

type Session struct {
	sni  string
	sess *yamux.Session
}

func RemoveIndex(s []*Session, index int) []*Session {
	return append(s[:index], s[index+1:]...)
}

func peekClientHello(reader io.Reader) (*tls.ClientHelloInfo, io.Reader, error) {
	peekedBytes := new(bytes.Buffer)
	hello, err := readClientHello(io.TeeReader(reader, peekedBytes))
	if err != nil {
		return nil, nil, err
	}
	return hello, io.MultiReader(peekedBytes, reader), nil
}

type readOnlyConn struct {
	reader io.Reader
}

func (conn readOnlyConn) Read(p []byte) (int, error)         { return conn.reader.Read(p) }
func (conn readOnlyConn) Write(p []byte) (int, error)        { return 0, io.ErrClosedPipe }
func (conn readOnlyConn) Close() error                       { return nil }
func (conn readOnlyConn) LocalAddr() net.Addr                { return nil }
func (conn readOnlyConn) RemoteAddr() net.Addr               { return nil }
func (conn readOnlyConn) SetDeadline(t time.Time) error      { return nil }
func (conn readOnlyConn) SetReadDeadline(t time.Time) error  { return nil }
func (conn readOnlyConn) SetWriteDeadline(t time.Time) error { return nil }

func readClientHello(reader io.Reader) (*tls.ClientHelloInfo, error) {
	var hello *tls.ClientHelloInfo

	err := tls.Server(readOnlyConn{reader: reader}, &tls.Config{
		GetConfigForClient: func(argHello *tls.ClientHelloInfo) (*tls.Config, error) {
			hello = new(tls.ClientHelloInfo)
			*hello = *argHello
			return nil, nil
		},
	}).Handshake()

	if hello == nil {
		return nil, err
	}

	return hello, nil
}
