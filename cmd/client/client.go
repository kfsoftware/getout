package client

import (
	"encoding/binary"
	"fmt"
	"github.com/hashicorp/yamux"
	log "github.com/schollz/logger"
	"github.com/spf13/cobra"
	"io"
	"net"
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
func (c *clientCmd) run() error {
	conn, err := net.Dial("tcp", c.tunnel)
	if err != nil {
		panic(err)
	}
	session, err := yamux.Client(conn, nil)
	if err != nil {
		panic(err)
	}
	initialConn, err := session.Open()
	if err != nil {
		panic(err)
	}
	buffer := []byte(c.sni)
	err = binary.Write(initialConn, binary.LittleEndian, int64(len(buffer)))
	if err != nil {
		panic(err)
	}
	if _, err = initialConn.Write(buffer); err != nil {
		return err
	}
	initialConn.Close()
	log.Infof("Connection established, waiting for connections..")
	for {
		conn, err := session.Accept()
		if err != nil {
			log.Tracef("Failed to accept connections: %v", err)
			return err
		}
		destConn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", c.host, c.port))
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
func NewClientCmd() *cobra.Command {
	c := &clientCmd{}
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
	persistentFlags.StringVarP(&c.sni, "sni", "", "", "SNI Host to listen for")
	persistentFlags.StringVarP(&c.tunnel, "tunnel", "", "tunnel.arise.kungfusoftware.es:8082", "Tunnel to connect to")
	persistentFlags.IntVarP(&c.port, "port", "", 0, "Local port to redirect to")
	persistentFlags.StringVarP(&c.host, "host", "", "localhost", "Local host to redirect to")

	cmd.MarkPersistentFlagRequired("sni")
	cmd.MarkPersistentFlagRequired("port")
	return cmd
}
