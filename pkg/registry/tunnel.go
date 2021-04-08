package registry

import (
	"crypto/tls"
	"encoding/binary"
	"github.com/golang/protobuf/proto"
	"github.com/hashicorp/yamux"
	"github.com/kfsoftware/getout/pkg/db"
	"github.com/kfsoftware/getout/pkg/messages"
	uuid "github.com/satori/go.uuid"
	log "github.com/schollz/logger"
	"gorm.io/gorm"
	"net"
	"net/http"
)

type TunnelRegistry struct {
	sessions []*Session
	db       *gorm.DB
}
type Protocol string

const (
	HttpProtocol Protocol = "http"
	TlsProtocol  Protocol = "tls"
)

type Tunnel interface {
	Match(conn net.Conn, req *http.Request, clientHello *tls.ClientHelloInfo) bool
}

type HttpTunnel struct {
	host string
}

func (t *HttpTunnel) Match(conn net.Conn, req *http.Request) bool {
	return req.Host == t.host
}

type TlsTunnel struct {
	sni string
}

func (t *TlsTunnel) Match(conn net.Conn, req *http.Request, clientHello *tls.ClientHelloInfo) bool {
	return clientHello.ServerName == t.sni
}

type TlsProps struct {
	Sni string
}
type HttpProps struct {
	Host string
}
type Session struct {
	sess *yamux.Session
}

func NewTunnelRegistry() *TunnelRegistry {
	return &TunnelRegistry{
		sessions: []*Session{},
	}
}
func (r *TunnelRegistry) StoreSession(
	sess *yamux.Session,
) error {
	initialConn, err := sess.Accept()
	if err != nil {
		log.Warnf("Failed to accept connection: %v", err)
		return err
	}
	var sz int64
	err = binary.Read(initialConn, binary.LittleEndian, &sz)
	reqBytes := make([]byte, sz)
	n, err := initialConn.Read(reqBytes)
	log.Debugf("Read message %s %d", reqBytes, n)
	if err != nil {
		log.Warnf("Failed to read initial connection: %v", err)
		return err
	}
	tunnelReq := &messages.TunnelRequest{}
	err = proto.Unmarshal(reqBytes, tunnelReq)
	if err != nil {
		return err
	}

	if tunnelReq.GetTls() != nil {
		log.Infof("Received tls tunnel request")
	} else if tunnelReq.GetHttp() != nil {
		log.Infof("Received http tunnel request host=%s", tunnelReq.GetHttp().Host)
	}
	r.sessions = append(r.sessions, &Session{sess: sess})

	return nil
}
func (r *TunnelRegistry) saveSession(conn net.Conn, url string, protocol db.Protocol) (*db.Tunnel, error) {
	tunnel := &db.Tunnel{
		ID:       uuid.NewV4().String(),
		URL:      url,
		ClientIP: conn.RemoteAddr().String(),
		Protocol: protocol,
		Data:     nil,
		Metadata: nil,
		Active:   true,
	}
	result := r.db.Create(tunnel)
	if result.Error != nil {
		return nil, result.Error
	}
	return tunnel, nil
}

func (r *TunnelRegistry) GetSession(conn net.Conn) (net.Conn, error) {
	sess := r.sessions[0].sess
	destConn, err := sess.Open()
	if err != nil {
		log.Warnf("Connection closed")
		return nil, err
	}
	return destConn, nil
}
