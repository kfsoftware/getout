package server

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/inconshreveable/go-vhost"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/kfsoftware/getout/pkg/messages"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

type serverCmd struct {
	tunnelAddr string
	adminAddr  string
	addr       string
}

func (c *serverCmd) validate() error {
	return nil
}

func (c serverCmd) returnResponse(initialConn net.Conn, status messages.TunnelStatus) error {
	tunnelResponse := &messages.TunnelResponse{Status: status}
	log.Debug().Msgf("Returning response to client: %s", status)
	err := messages.WriteMsg(initialConn, tunnelResponse)
	if err != nil {
		return err
	}
	err = initialConn.Close()
	if err != nil {
		return err
	}
	return nil
}
func (c serverCmd) run() error {
	l, _ := net.Listen("tcp", c.addr)
	muxTimeout := time.Second * 5
	// start multiplexing on it
	mux, err := vhost.NewTLSMuxer(l, muxTimeout)
	if err != nil {
		log.Err(err).Msg("failed to create muxer")
	}
	log.Debug().Msgf("Starting server %s", c.addr)
	muxServer, err := net.Listen("tcp", c.tunnelAddr)
	if err != nil {
		panic(fmt.Errorf("error listening on %s: %w", c.tunnelAddr, err))
	}
	defer func(muxServer net.Listener) {
		_ = muxServer.Close()
	}(muxServer)
	sessions := map[string]Session{}
	var mutex sync.RWMutex // protects multiple tunnels started at the same time

	go func() {
		log.Info().Msgf("tunnel listening on %s", c.tunnelAddr)
		for {
			conn, err := muxServer.Accept()
			if err != nil {
				log.Warn().Msgf("Connection closed")
				return
			}
			mutex.Lock()

			log.Trace().Msgf("client %s connected", conn.RemoteAddr().String())
			sess, err := yamux.Server(conn, nil)
			if err != nil {
				log.Err(err).Msg("failed to create yamux session")
				mutex.Unlock()
				continue
			}
			initialConn, err := sess.Accept()
			if err != nil {
				mutex.Unlock()
				log.Trace().Msgf("client %s disconnected", conn.RemoteAddr().String())
				if initialConn != nil {
					err = c.returnResponse(initialConn, messages.TunnelStatus_ERROR)
					if err != nil {
						log.Warn().Msgf("Failed to send response: %v", err)
					}
				}
				continue
			}
			msg := &messages.TunnelRequest{}
			err = messages.ReadMsgInto(initialConn, msg)
			if err != nil {
				mutex.Unlock()
				log.Trace().Msgf("failed to read message", conn.RemoteAddr().String())
				err = c.returnResponse(initialConn, messages.TunnelStatus_ERROR)
				if err != nil {
					log.Warn().Msgf("Failed to send response: %v", err)
				}
				continue
			}

			sni := msg.GetTls().GetSni()
			log.Debug().Msgf("Received request for %v", sni)
			muxListener, err := mux.Listen(sni)
			if err != nil {
				mutex.Unlock()
				log.Err(err).Msgf("failed to listen on %s", sni)
				if strings.Contains(strings.ToLower(err.Error()), "already bound") {
					err = c.returnResponse(initialConn, messages.TunnelStatus_ALREADY_EXISTS)
					if err != nil {
						log.Warn().Msgf("Failed to send response: %v", err)
					}
					continue
				}
				continue
			}
			sessions[sni] = Session{
				SNI:        sni,
				RemoteAddr: conn.RemoteAddr().String(),
				LocalAddr:  muxListener.Addr().String(),
			}
			err = c.returnResponse(initialConn, messages.TunnelStatus_OK)
			if err != nil {
				mutex.Unlock()
				_ = muxListener.Close()
				delete(sessions, sni)
				log.Trace().Msgf("failed to write message", conn.RemoteAddr().String())
				err = c.returnResponse(initialConn, messages.TunnelStatus_ERROR)
				if err != nil {
					log.Warn().Msgf("Failed to send response: %v", err)
				}
				continue
			}
			err = initialConn.Close()
			if err != nil {
				mutex.Unlock()
				_ = muxListener.Close()
				delete(sessions, sni)
				log.Trace().Msgf("failed to close connection", conn.RemoteAddr().String())
				err = c.returnResponse(initialConn, messages.TunnelStatus_ERROR)
				if err != nil {
					log.Warn().Msgf("Failed to send response: %v", err)
				}
				continue
			}
			mutex.Unlock()
			go func(ml net.Listener) {
				defer func() {
					delete(sessions, sni)
					if r := recover(); r != nil {
						log.Info().Msgf("Recovered in request dispatcher", r)
					}
				}()
				for {
					conn, err := ml.Accept()
					if err != nil {
						log.Err(err).Msg("Error accepting connection")
						delete(sessions, sni)
						_ = muxListener.Close()
						if strings.Contains(strings.ToLower(err.Error()), "listener closed") {
							log.Info().Msg("listener closed")
							return
						}
						continue
					}
					destConn, err := sess.Open()
					if err != nil {
						_ = conn.Close()
						log.Warn().Msgf("Connection closed")
						continue
					}
					var wg sync.WaitGroup
					wg.Add(2)
					transfer := func(side string, dst, src net.Conn) {
						log.Trace().Msgf("proxing %s -> %s", src.RemoteAddr(), dst.RemoteAddr())
						tStart := time.Now()

						n, err := io.Copy(dst, src)
						if err != nil {
							log.Error().Msgf("%s: copy error: %s", side, err)
						}

						if err := src.Close(); err != nil {
							log.Trace().Msgf("%s: close error: %s", side, err)
						}

						// not for yamux streams, but for client to local server connections
						if d, ok := dst.(*net.TCPConn); ok {
							if err := d.CloseWrite(); err != nil {
								log.Trace().Msgf("%s: closeWrite error: %s", side, err)
							}
						}
						wg.Done()
						log.Trace().Msgf("done proxing %s -> %s: %d bytes in %s", src.RemoteAddr(), dst.RemoteAddr(), n, time.Since(tStart))
					}
					go transfer("remote to local", conn, destConn)
					go transfer("local to remote", destConn, conn)
				}
			}(muxListener)
			go func() {
				log.Debug().Msgf("Checking if session %s is alive", sni)
				for {
					_, err = sess.Ping()
					if err != nil {
						log.Info().Msgf("Session %s inactive, removing it: %v", sni, err)
						delete(sessions, sni)
						err = muxListener.Close()
						if err != nil {
							log.Err(err).Msgf("Failed to close listener")
						}
						break
					}
					time.Sleep(2 * time.Second)
					continue
				}
			}()
		}
	}()
	go func() {
		r := gin.Default()
		r.GET("/tunnels", func(c *gin.Context) {
			c.JSON(200, sessions)
		})
		log.Info().Msgf("admin server listening on %s", c.adminAddr)
		err := r.Run(c.adminAddr)
		if err != nil {
			log.Error().Msgf("failed to listen on address: %s %v", c.adminAddr, err)
		}
	}()
	go func() {
		for {
			conn, err := mux.NextError()
			switch err.(type) {
			case vhost.BadRequest:
				log.Trace().Msgf("got a bad request!")
			case vhost.NotFound:
				log.Trace().Msgf("got a connection for an unknown vhost")
			case vhost.Closed:
				log.Trace().Msgf("closed conn: %s", err)
			default:
				log.Trace().Msgf("Server error")
			}

			if conn != nil {
				_ = conn.Close()
			}
		}
	}()
	select {} // block forever
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

	_ = cmd.MarkPersistentFlagRequired("addr")
	_ = cmd.MarkPersistentFlagRequired("tunnel-addr")
	_ = cmd.MarkPersistentFlagRequired("admin-addr")
	return cmd
}

type Session struct {
	SNI        string `json:"sni"`
	RemoteAddr string `json:"remoteAddr"`
	LocalAddr  string `json:"localAddr"`
}
