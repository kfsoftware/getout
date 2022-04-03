package client

import (
	"encoding/binary"
	"fmt"
	"github.com/golang/protobuf/proto"
	"github.com/hashicorp/yamux"
	"github.com/kfsoftware/getout/pkg/messages"
	"github.com/rs/zerolog/log"
	"io"
	"net"
	"sync"
	"time"
)

func NewTunnelClient(tunnelAddress string) *tunnelClient {
	return &tunnelClient{
		tunnelAddress: tunnelAddress,
	}
}

type tunnelClient struct {
	tunnelAddress string
}

func (c *tunnelClient) StartTlsTunnel(sni string, remoteAddress string) error {
	log.Debug().Msgf("Starting TLS tunnel to %s", c.tunnelAddress)
	conn, err := net.Dial("tcp", c.tunnelAddress)
	if err != nil {
		log.Trace().Msgf("Failed to connect to tunnel: %v", err)
		return err
	}
	session, err := yamux.Client(conn, nil)
	if err != nil {
		log.Trace().Msgf("Failed to create yamux session: %v", err)
		return err
	}
	tunnelReq := &messages.TunnelRequest{
		Sni: sni,
	}
	initialConn, err := session.Open()
	if err != nil {
		return err
	}
	b, err := proto.Marshal(tunnelReq)
	if err != nil {
		return err
	}
	err = binary.Write(initialConn, binary.LittleEndian, int64(len(b)))
	if err != nil {
		return err
	}
	if _, err = initialConn.Write(b); err != nil {
		return err
	}
	err = initialConn.Close()
	if err != nil {
		return err
	}
	err = c.startSNIProxy(session, remoteAddress)
	if err != nil {
		return err
	}
	return nil
}

func (c tunnelClient) startSNIProxy(session *yamux.Session, remoteAddress string) error {
	log.Debug().Msgf("Starting SNI proxy for %s", remoteAddress)
	for {
		conn, err := session.Accept()
		if err != nil {
			log.Trace().Msgf("Failed to accept connections: %v", err)
			return err
		}
		destConn, err := net.DialTimeout("tcp", remoteAddress, time.Second*5)
		if err != nil {
			log.Trace().Msgf("Failed to connect to remote address: %v", err)
			conn.Write([]byte("Failed to connect to remote address"))
			connCloseErr := conn.Close()
			if connCloseErr != nil {
				log.Trace().Msgf("Failed to close connection: %v", connCloseErr)
			}
			if destConn != nil {
				destConn.Close()
			}
			return err
		}
		log.Debug().Msgf("client %s connected", conn.RemoteAddr().String())
		copyConn := func(writer, reader net.Conn) {
			defer writer.Close()
			defer reader.Close()
			_, err := io.Copy(writer, reader)
			if err != nil {
				fmt.Printf("io.Copy error: %s", err)
			}
			log.Info().Msgf("Connection finished")
		}
		_ = copyConn
		var wg sync.WaitGroup
		wg.Add(2)

		transfer := func(side string, dst, src net.Conn) {
			log.Debug().Msgf("proxing %s -> %s", src.RemoteAddr(), dst.RemoteAddr())

			n, err := io.Copy(dst, src)
			if err != nil {
				log.Error().Msgf("%s: copy error: %s", side, err)
			}

			if err := src.Close(); err != nil {
				log.Debug().Msgf("%s: close error: %s", side, err)
			}

			// not for yamux streams, but for client to local server connections
			if d, ok := dst.(*net.TCPConn); ok {
				if err := d.CloseWrite(); err != nil {
					log.Debug().Msgf("%s: closeWrite error: %s", side, err)
				}

			}
			wg.Done()
			log.Debug().Msgf("done proxing %s -> %s: %d bytes", src.RemoteAddr(), dst.RemoteAddr(), n)
		}

		go transfer("remote to local", conn, destConn)
		go transfer("local to remote", destConn, conn)
	}
}