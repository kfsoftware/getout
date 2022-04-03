package server

import (
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type serverCmd struct {
	tunnelAddr string
	adminAddr  string
	addr       string
}

func (c *serverCmd) validate() error {
	return nil
}
func getDb(datasourceName string) *gorm.DB {
	os.Setenv("TZ", "UTC")
	gormConfig := &gorm.Config{}
	dbClient, err := gorm.Open(
		postgres.New(
			postgres.Config{
				DSN:                  datasourceName,
				PreferSimpleProtocol: true,
			},
		),
		gormConfig,
	)
	if err != nil {
		panic(err)
	}
	err = dbClient.AutoMigrate(&db.Tunnel{})
	if err != nil {
		panic(err)
	}
	return dbClient
}
func (c *serverCmd) run() error {
	clientDb := getDb(c.postgresUrl)
	tunnelRegistry := registry.NewTunnelRegistry(clientDb)
	crt, err := tls.LoadX509KeyPair(c.tlsCrt, c.tlsKey)
	if err != nil {
		return err
	}
	serverListener, err := net.Listen("tcp", c.addr)
	if err != nil {
		return err
	}
	defer muxServer.Close()
	//adminServer, err := net.Listen("tcp", c.adminAddr)
	//if err != nil {
	//	panic(fmt.Errorf("error listening on %s: %w", c.tunnelAddr, err))
	//}
	//defer adminServer.Close()
	var sessions []*Session
	go func() {
		log.Info().Msgf("tunnel listening on %s", c.tunnelAddr)
		for {
			conn, err := muxServer.Accept()
			if err != nil {
				log.Warn().Msgf("Connection closed")
				return
			}
			log.Debug().Msgf("client %s connected", conn.RemoteAddr().String())
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
			log.Debug().Msgf("Read message %s %d", sni, n)
			if err != nil {
				panic(err)
			}
			sessions = append(sessions, &Session{
				SNI:        string(sni),
				RemoteAddr: conn.RemoteAddr().String(),
				LocalAddr:  conn.LocalAddr().String(),
				sess:       sess,
			})
		}
	}()
	go func() {
		log.Info().Msgf("admin listening on %s", c.adminAddr)
		http.HandleFunc("/tunnels", func(writer http.ResponseWriter, request *http.Request) {
			sessionsBytes, err := json.Marshal(sessions)
			if err != nil {
				writer.Header().Set("Content-Type", "application/json")
				writer.Write([]byte("Error"))
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.Write(sessionsBytes)
		})

		err := http.ListenAndServe(c.adminAddr, nil)
		if err != nil {
			log.Error().Msgf("Error listening on %s: %w", c.adminAddr, err)
		}

	}()
	for {
		log.Info().Msgf("listening requests %s", c.addr)
		conn, err := server.Accept()
		if err != nil {
			panic(err)
		}
		log.Debug().Msgf("client %s connected", conn.RemoteAddr().String())

		clientHello, originalConn, err := peekClientHello(conn)
		if err != nil {
			log.Error().Msgf("Error extracting client hello %v", err)
		}
		_ = originalConn
		sni := clientHello.ServerName
		//sni := "localhost"
		log.Info().Msgf("SNI=%s", sni)
		if len(sessions) == 0 {
			conn.Close()
			continue
		}
		var destSess *Session
		for _, session := range sessions {
			if session.SNI == sni {
				destSess = session
			}
		}
		if destSess == nil {
			log.Warn().Msgf("Session not found")
			conn.Close()
			continue
		}
		destConn, err := destSess.sess.Open()
		if err != nil {
			conn.Close()
			sessions = RemoveIndex(sessions, 0)
			log.Warn().Msgf("Connection closed")
			continue
		}
		var wg sync.WaitGroup
		wg.Add(2)
		copyConn := func(writer net.Conn, reader net.Conn) {
			defer func() {
				log.Trace().Msg("Closing copyConn connections")
				writer.Close()
				reader.Close()
			}()
			_, err := io.Copy(writer, reader)
			if err != nil {
				log.Trace().Msgf("io.Copy error: %s", err)
			}
			log.Info().Msgf("Connection finished")
		}
		copyStream := func(side string, dst net.Conn, src io.Reader) {
			log.Debug().Msgf("proxing  -> %s", dst.RemoteAddr())
			n, err := io.Copy(dst, src)
			if err != nil {
				log.Error().Msgf("%s: copy error: %s", side, err)
			}

			// not for yamux streams, but for client to local server connections
			if d, ok := dst.(*net.TCPConn); ok {
				if err := d.CloseWrite(); err != nil {
					log.Debug().Msgf("%s: closeWrite error: %s", side, err)
				}
			}
			wg.Done()
			log.Debug().Msgf("done proxing -> %s: %d bytes", dst.RemoteAddr(), n)

		}
		_ = copyConn
		_ = copyStream

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
		go copyStream("local to remote", destConn, originalConn)
	}
	adminListener, err := net.Listen("tcp", c.adminAddr)
	if err != nil {
		return err
	}
	i := tunnel.NewTunnelServerInstance(
		tunnelRegistry,
		tunnelListener,
		serverListener,
		adminListener,
		c.defaultDomain,
		[]tls.Certificate{crt},
	)
	err = i.Start()
	if err != nil {
		return err
	}
	return nil
}

func NewServerCmd() *cobra.Command {
	c := &serverCmd{}
	cmd := &cobra.Command{
		Use: "server",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.validate(); err != nil {
				return err
			}
			return c.run()
		},
	}
	persistentFlags := cmd.Flags()
	persistentFlags.StringVarP(&c.addr, "addr", "", "", "Address to listen for requests")
	persistentFlags.StringVarP(&c.tunnelAddr, "tunnel-addr", "", "", "Address to manage the tunnel connections")
	persistentFlags.StringVarP(&c.adminAddr, "admin-addr", "", "127.0.0.1:8003", "Address for admin utilities")

	cmd.MarkPersistentFlagRequired("addr")
	cmd.MarkPersistentFlagRequired("tunnel-addr")
	cmd.MarkPersistentFlagRequired("admin-addr")
	return cmd
}

type Session struct {
	SNI        string `json:"sni"`
	RemoteAddr string `json:"remoteAddr"`
	sess       *yamux.Session
	LocalAddr  string `json:"localAddr"`
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
