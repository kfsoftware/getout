package tunnel

import (
	"encoding/binary"
	"fmt"
	"github.com/golang/protobuf/proto"
	"github.com/hashicorp/yamux"
	"github.com/kfsoftware/getout/pkg/messages"
	"github.com/kfsoftware/getout/pkg/registry"
	log "github.com/schollz/logger"
	"io"
	"net"
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
		//  fmt.Sprintf("%s:%d", c.host, c.port)
		if err != nil {
			panic(err)
		}
		log.Debugf("client %s connected", conn.RemoteAddr().String())
		copyConn := func(writer, reader net.Conn) {
			defer writer.Close()
			//defer reader.Close()
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

func (t *instance) startMainServer(server net.Listener) {
	for {
		conn, err := server.Accept()
		if err != nil {
			panic(err)
		}
		log.Debugf("client %s connected", conn.RemoteAddr().String())
		//
		//clientHello, originalConn, err := peekClientHello(conn)
		//if err != nil {
		//	log.Errorf("Error extracting client hello %v", err)
		//}
		//sni := clientHello.ServerName
		//log.Infof("SNI=%s", sni)
		//if len(sessions) == 0 {
		//	conn.Close()
		//	continue
		//}
		//session, exists := sessions[sni]
		//
		//if !exists {
		//	log.Warnf("Session not found")
		//	conn.Close()
		//	continue
		//}
		//destConn, err := session.sess.Open()
		//if err != nil {
		//	conn.Close()
		//	delete(sessions, sni)
		//	log.Warnf("Connection closed")
		//	continue
		//}
		destConn, err := t.registry.GetSession(conn)
		if err != nil {
			conn.Close()
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

		go copyStream(destConn, conn)
		go copyConn(conn, destConn)
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
		//initialConn, err := sess.Accept()
		//if err != nil {
		//	log.Warnf("Failed to accept connection: %v", err)
		//	continue
		//}
		//var sz int64
		//err = binary.Read(initialConn, binary.LittleEndian, &sz)
		//sni := make([]byte, sz)
		//n, err := initialConn.Read(sni)
		//log.Debugf("Read message %s %d", sni, n)
		//if err != nil {
		//	log.Warnf("Failed to read initial connection: %v", err)
		//	continue
		//}
		err = t.registry.StoreSession(
			sess,
			//sess,
			//registry.TlsProps{Sni: string(sni)},
		)
		if err != nil {
			log.Warnf("Couldn't store session: %v", err)
			continue
		}
	}
}
