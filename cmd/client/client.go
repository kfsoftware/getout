package client

import (
	"encoding/binary"
	"fmt"
	"github.com/hashicorp/yamux"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"io"
	"net"
	"sync"
	"time"
)

type clientCmd struct {
	sni    string
	port   int
	tunnel string
	host   string
}

func (c *clientCmd) validate() error {
	return nil
}
func startTunnel(session *yamux.Session, remoteAddress string) error {
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
				log.Warnf("io.Copy error: %s", err)
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

		//wg.Wait()
		//go copyConn(conn, destConn)
		//go copyConn(destConn, conn)
	}
}
func (c *clientCmd) setupInitialConn() (*yamux.Session, error) {
	conn, err := net.Dial("tcp", c.tunnel)
	if err != nil {
		log.Trace().Msgf("Failed to connect to tunnel: %v", err)
		return nil, err
	}
	session, err := yamux.Client(conn, nil)
	if err != nil {
		log.Trace().Msgf("Failed to create yamux session: %v", err)
		return nil, err
	}
	initialConn, err := session.Open()
	if err != nil {
		log.Trace().Msgf("Failed to open initial connection: %v", err)
		return nil, err
	}
	buffer := []byte(c.sni)
	err = binary.Write(initialConn, binary.LittleEndian, int64(len(buffer)))
	if err != nil {
		log.Trace().Msgf("Failed to write length of sni: %v", err)
		return nil, err
	}
	if _, err = initialConn.Write(buffer); err != nil {
		return nil, err
	}
	err = initialConn.Close()
	if err != nil {
		return nil, err
	}
	log.Info().Msgf("Connection established, waiting for connections..")
	return session, nil
}
func (c *clientCmd) run() error {
	remoteAddress := fmt.Sprintf("%s:%d", c.host, c.port)
	for {
		session, err := c.setupInitialConn()
		if err != nil {
			log.Error().Msgf("Failed to start tunnel: %v retrying in %v", err, 5*time.Second)
			time.Sleep(5 * time.Second)
			continue
		}
		err = startTunnel(session, remoteAddress)
		if err != nil {
			log.Error().Msgf("Failed to start tunnel: %v retrying in %v", err, 5*time.Second)
			time.Sleep(5 * time.Second)
			continue
		} else {
			log.Info().Msgf("Tunnel started")
			break
		}

	}
	return nil
}
func NewClientCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "client",
	}
	cmd.AddCommand(newHttpCmd(), newHttpsCmd(), newTlsCmd())
	return cmd
}
