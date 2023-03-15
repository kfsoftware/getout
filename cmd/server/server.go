package server

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/inconshreveable/go-vhost"
	"github.com/kfsoftware/getout/proxy"
	"github.com/pkg/errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/kfsoftware/getout/log"
	"github.com/kfsoftware/getout/pkg/messages"
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
	log.Debugf("Returning response to client: %s", status)
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

func (s *Session) cleanup() {
	s.Lock()
	defer s.Unlock()
	defer func() {
		s.cleanupDone = true
	}()
	if s.cleanupDone {
		return
	}
	if s.Conn != nil {
		s.Conn.Close()
	}
	if s.Sess != nil && !s.Sess.IsClosed() {
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
		log.Warnf("Session already exists for %s", sni)
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
func (r *SessionRegistry) cleanupDeadSessions() {
	for sni, session := range r.sessions {
		if session.Sess.IsClosed() {
			log.Debugf("Session closed for %s", sni)
			r.delete(sni)
		}
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
	defer conn.Close()
	log.Debugf("client %s connected", conn.RemoteAddr().String())
	config := yamux.DefaultConfig()
	// setup session
	sess, err := yamux.Server(conn, config)
	if err != nil {
		log.Errorf("failed to create yamux session")
		return err
	}
	defer sess.Close()
	// accept connection
	initialConn, err := sess.Accept()
	if err != nil {
		log.Debugf("client %s disconnected", conn.RemoteAddr().String())
		log.Errorf("multiplex conn accept failed %v", err)
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
		log.Debugf("Session already exists in the registry for %s", sni)
		err = c.returnResponse(initialConn, messages.TunnelStatus_ALREADY_EXISTS)
		if err != nil {
			log.Warnf("Failed to send response: %v", err)
		}
		return err
	}
	muxListener, err := c.startMuxListener(mux, initialConn, sni)
	if err != nil {
		if muxListener != nil {
			muxListener.Close()
		}
		if msg != nil {
			log.Errorf("failed to listen on %s", msg.GetTls().GetSni())
		}
		if strings.Contains(strings.ToLower(err.Error()), "already bound") {
			err = c.returnResponse(initialConn, messages.TunnelStatus_ALREADY_EXISTS)
			if err != nil {
				log.Warnf("Failed to send response: %v", err)
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
	defer func() {
		c.sessionRegistry.delete(sni)
	}()
	err = c.sessionRegistry.store(sni, session)
	if err != nil {
		log.Errorf("failed to store session for %s", sni)
		return err
	}
	err = c.returnResponse(initialConn, messages.TunnelStatus_OK)
	if err != nil {
		err = c.returnResponse(initialConn, messages.TunnelStatus_ERROR)
		if err != nil {
			log.Warnf("Failed to send response: %v", err)
		}
		return err
	}
	go func() {
		log.Debugf("Checking if session %s is alive", sni)
		for {
			_, err = sess.Ping()
			if err != nil {
				log.Infof("Session %s inactive, removing it: %v", sni, err)
				break
			}
			time.Sleep(2 * time.Second)
			continue
		}
	}()
	for {
		conn, err := muxListener.Accept()
		if err != nil {
			log.Errorf("Error accepting connection", err)
			if strings.Contains(strings.ToLower(err.Error()), "listener closed") {
				log.Info("listener closed")
				return errors.New("listener closed")
			}
			continue
		}
		destConn, err := sess.Open()
		if err != nil {
			_ = conn.Close()
			log.Warnf("Connection closed")
			continue
		}
		p := proxy.New(
			conn,
			destConn,
		)
		p.Start()
	}

	return nil
}

func (c *serverCmd) startMuxListener(mux *vhost.TLSMuxer, initialConn net.Conn, sni string) (net.Listener, error) {
	log.Debugf("Received request for %v", sni)
	muxListener, err := mux.Listen(sni)
	if err != nil {
		log.Errorf("failed to listen on %s", sni)
		if strings.Contains(strings.ToLower(err.Error()), "already bound") {
			respErr := c.returnResponse(initialConn, messages.TunnelStatus_ALREADY_EXISTS)
			if respErr != nil {
				log.Warnf("Failed to send response: %v", err)
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
			log.Warnf("Failed to send response: %v", err)
		}
		return muxListener, err
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
		log.Errorf("failed to create muxer")
	}
	log.Debugf("Starting server %s", c.addr)
	muxServer, err := net.Listen("tcp", c.tunnelAddr)
	if err != nil {
		panic(fmt.Errorf("error listening on %s: %w", c.tunnelAddr, err))
	}
	go func() {
		log.Infof("tunnel listening on %s", c.tunnelAddr)
		defer func() {
			if r := recover(); r != nil {
				log.Infof("tunnel listener closed %v", r)
			}
		}()
		for {
			conn, err := muxServer.Accept()
			if err != nil {
				log.Warnf("Connection closed")
				return
			}
			log.Debugf("Accepted connection from %s", conn.RemoteAddr())
			go func() {
				err = c.handleTunnelRequest(mux, conn)
				if err != nil {
					log.Warnf("Failed to handle tunnel request: %v", err)
				}
			}()
		}
		log.Info("tunnel server closed")
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
		log.Infof("admin server listening on %s", c.adminAddr)
		err := r.Run(c.adminAddr)
		if err != nil {
			log.Errorf("failed to listen on address: %s %v", c.adminAddr, err)
		}
	}()
	go func() {
		for {
			conn, err := mux.NextError()
			switch err.(type) {
			case vhost.BadRequest:
				log.Debugf("got a bad request!")
			case vhost.NotFound:
				log.Debugf("got a connection for an unknown vhost")
			case vhost.Closed:
				log.Debugf("closed conn: %s", err)
			default:
				log.Debugf("Server error")
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
	sync.RWMutex
	cleanupDone bool
	SNI         string `json:"sni"`
	//RemoteAddr string `json:"remoteAddr"`
	//LocalAddr  string `json:"localAddr"`
	Conn net.Conn
	Mux  net.Listener
	Sess *yamux.Session
}
