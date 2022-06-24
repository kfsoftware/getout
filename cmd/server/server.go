package server

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/kfsoftware/go-vhost"
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
	tunnelAddr      string
	adminAddr       string
	addr            string
	sessionRegistry *SessionRegistry
}

func (c *serverCmd) validate() error {
	return nil
}

func (c *serverCmd) returnResponse(initialConn net.Conn, status messages.TunnelStatus) error {
	tunnelResponse := &messages.TunnelResponse{Status: status}
	log.Debug().Msgf("Returning response to client: %s", status)
	err := messages.WriteMsg(initialConn, tunnelResponse)
	if err != nil {
		return err
	}
	return nil
}

type SessionRegistry struct {
	sync.RWMutex
	sessions map[string]*Session
}

func (s Session) cleanup() {
	if s.Conn != nil {
		s.Conn.Close()
	}
	if s.Sess != nil {
		s.Sess.Close()
	}
	if s.Mux != nil {
		s.Mux.Close()
	}
}
func (r *SessionRegistry) store(sni string, s *Session) error {
	r.Lock()
	defer r.Unlock()
	_, ok := r.sessions[sni]
	if ok {
		log.Warn().Msgf("Session already exists for %s", sni)
		return fmt.Errorf("session already exists for %s", sni)
	}
	r.sessions[sni] = s
	return nil
}

func (r *SessionRegistry) delete(sni string) {
	r.Lock()
	defer r.Unlock()
	s, ok := r.sessions[sni]
	if ok {
		s.cleanup()
		delete(r.sessions, sni)
	}
}

func (r *SessionRegistry) find(sni string) *Session {
	r.RLock()
	defer r.RUnlock()
	s, ok := r.sessions[sni]
	if !ok {
		return nil
	}
	return s
}
func (c *serverCmd) handleTunnelRequest(mux *vhost.TLSMuxer, conn net.Conn) error {
	log.Trace().Msgf("client %s connected", conn.RemoteAddr().String())
	config := yamux.DefaultConfig()
	sess, err := yamux.Server(conn, config)
	if err != nil {
		log.Err(err).Msg("failed to create yamux session")
		return err
	}
	initialConn, err := sess.Accept()
	if err != nil {
		log.Trace().Msgf("client %s disconnected", conn.RemoteAddr().String())
		if initialConn != nil {
			err = c.returnResponse(initialConn, messages.TunnelStatus_ERROR)
			if err != nil {
				log.Warn().Msgf("Failed to send response: %v", err)
			}
		}
		return err
	}
	defer initialConn.Close()
	msg := &messages.TunnelRequest{}
	err = messages.ReadMsgInto(initialConn, msg)
	if err != nil {
		return nil
	}
	sni := msg.GetTls().GetSni()
	s := c.sessionRegistry.find(sni)
	if s != nil {
		defer conn.Close()
		defer sess.Close()
		log.Trace().Msgf("Session already exists in the registry for %s", sni)
		err = c.returnResponse(initialConn, messages.TunnelStatus_ALREADY_EXISTS)
		if err != nil {
			log.Warn().Msgf("Failed to send response: %v", err)
		}
		return err
	}
	muxListener, err := c.startMuxListener(mux, initialConn, sni)
	if err != nil {
		if msg != nil {
			log.Err(err).Msgf("failed to listen on %s", msg.GetTls().GetSni())
		}
		if muxListener != nil {
			muxListener.Close()
		}
		defer conn.Close()
		defer sess.Close()
		if strings.Contains(strings.ToLower(err.Error()), "already bound") {
			err = c.returnResponse(initialConn, messages.TunnelStatus_ALREADY_EXISTS)
			if err != nil {
				log.Warn().Msgf("Failed to send response: %v", err)
			}
			return err
		}
		return err
	}
	session := &Session{
		SNI:  sni,
		Conn: conn,
		Mux:  muxListener,
		Sess: sess,
	}
	err = c.sessionRegistry.store(sni, session)
	if err != nil {
		log.Err(err).Msgf("failed to store session for %s", sni)
		session.cleanup()
		return err
	}
	err = c.returnResponse(initialConn, messages.TunnelStatus_OK)
	if err != nil {
		err = c.returnResponse(initialConn, messages.TunnelStatus_ERROR)
		if err != nil {
			log.Warn().Msgf("Failed to send response: %v", err)
		}
		c.sessionRegistry.delete(sni)
		return err
	}
	go func(ml net.Listener) {
		defer func() {
			c.sessionRegistry.delete(sni)
			if r := recover(); r != nil {
				log.Info().Msgf("Recovered in request dispatcher", r)
			}
		}()
		for {
			conn, err := ml.Accept()
			if err != nil {
				log.Err(err).Msg("Error accepting connection")
				c.sessionRegistry.delete(sni)
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
				log.Trace().Msgf("done proxing %s -> %s: %d bytes in %s", src.RemoteAddr(), dst.RemoteAddr(), n, time.Since(tStart))
			}
			go transfer("remote to local", conn, destConn)
			go transfer("local to remote", destConn, conn)
		}
	}(muxListener)
	go func() {
		log.Debug().Msgf("Checking if session %s is alive", sni)
		defer func() {
			c.sessionRegistry.delete(sni)
		}()
		for {
			_, err = sess.Ping()
			if err != nil {
				log.Info().Msgf("Session %s inactive, removing it: %v", sni, err)
				c.sessionRegistry.delete(sni)
				break
			}
			time.Sleep(2 * time.Second)
			continue
		}
	}()
	return nil
}

func (c *serverCmd) startMuxListener(mux *vhost.TLSMuxer, initialConn net.Conn, sni string) (net.Listener, error) {

	log.Debug().Msgf("Received request for %v", sni)
	muxListener, err := mux.Listen(sni)
	if err != nil {
		log.Err(err).Msgf("failed to listen on %s", sni)
		if strings.Contains(strings.ToLower(err.Error()), "already bound") {
			err = mux.Del(sni)
			if err != nil {
				log.Err(err).Msgf("failed to delete mux %s", sni)
			}
			respErr := c.returnResponse(initialConn, messages.TunnelStatus_ALREADY_EXISTS)
			if respErr != nil {
				log.Warn().Msgf("Failed to send response: %v", err)
				return muxListener, respErr
			}
			return muxListener, err
		}
		return muxListener, err
	}
	err = c.returnResponse(initialConn, messages.TunnelStatus_OK)
	if err != nil {
		err = c.returnResponse(initialConn, messages.TunnelStatus_ERROR)
		if err != nil {
			log.Warn().Msgf("Failed to send response: %v", err)
		}
		c.sessionRegistry.delete(sni)
		return nil, err
	}
	return muxListener, nil
}

func (c *serverCmd) run() error {
	l, err := net.Listen("tcp", c.addr)
	if err != nil {
		return err
	}
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
		log.Warn().Msgf("Closing mux server %s", muxServer.Addr())
		_ = muxServer.Close()
	}(muxServer)
	go func() {
		log.Info().Msgf("tunnel listening on %s", c.tunnelAddr)
		defer func() {
			if r := recover(); r != nil {
				log.Info().Msgf("tunnel listener closed", r)
			}
		}()
		for {
			conn, err := muxServer.Accept()
			if err != nil {
				log.Warn().Msgf("Connection closed")
				return
			}
			log.Trace().Msgf("Accepted connection from %s", conn.RemoteAddr())
			err = c.handleTunnelRequest(mux, conn)
			if err != nil {
				log.Warn().Msgf("Failed to handle tunnel request: %v", err)
			}
		}
	}()
	go func() {
		r := gin.Default()
		r.GET("/tunnels", func(c1 *gin.Context) {
			c1.JSON(200, c.sessionRegistry.sessions)
		})
		r.DELETE("/tunnels/:sni", func(c1 *gin.Context) {
			c.sessionRegistry.delete(c1.Param("sni"))
			c1.JSON(200, c.sessionRegistry.sessions)
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
	sessionRegistry := &SessionRegistry{sessions: make(map[string]*Session)}

	c := &serverCmd{
		sessionRegistry: sessionRegistry,
	}
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
	SNI string `json:"sni"`
	//RemoteAddr string `json:"remoteAddr"`
	//LocalAddr  string `json:"localAddr"`
	Conn net.Conn
	Mux  net.Listener
	Sess *yamux.Session
}
