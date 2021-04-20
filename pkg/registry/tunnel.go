package registry

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"github.com/golang/protobuf/proto"
	uuid "github.com/google/uuid"
	"github.com/hashicorp/yamux"
	"github.com/kfsoftware/getout/pkg/db"
	"github.com/kfsoftware/getout/pkg/messages"
	"github.com/pkg/errors"
	log "github.com/schollz/logger"
	"gorm.io/gorm"
	"net"
	"net/http"
	"strings"
)

type TunnelRegistry struct {
	tlsTunnels   []*TlsTunnel
	httpsTunnels []*HttpsTunnel
	db           *gorm.DB
}
type Protocol string

const (
	HttpProtocol Protocol = "http"
	TlsProtocol  Protocol = "tls"
)

type Tunnel interface {
	GetSession() *yamux.Session
	Match(req *http.Request, clientHello *tls.ClientHelloInfo) bool
}

type HttpsTunnel struct {
	host string
	sess *yamux.Session
}

func (t *HttpsTunnel) Match(req http.Header) bool {
	host := strings.Split(req.Get("Host"), ":")[0]
	return host == t.host
}
func (t *HttpsTunnel) GetSession() *yamux.Session {
	return t.sess
}

type TlsTunnel struct {
	sni  string
	sess *yamux.Session
}

func (t *TlsTunnel) Match(clientHello *tls.ClientHelloInfo) bool {
	if clientHello == nil {
		return false
	}
	return clientHello.ServerName == t.sni
}
func (t *TlsTunnel) GetSession() *yamux.Session {
	return t.sess
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

func NewTunnelRegistry(db *gorm.DB) *TunnelRegistry {
	return &TunnelRegistry{
		httpsTunnels: nil,
		tlsTunnels:   nil,
		db:           db,
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
	defer initialConn.Close()
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
	var url string
	var protocol db.Protocol
	if tunnelReq.GetTls() != nil {
		protocol = db.TlsProtocol
		url = tunnelReq.GetTls().GetSni()
		log.Infof("Received tls tunnel request %s", url)
		tlsTunnel := &TlsTunnel{
			sni:  url,
			sess: sess,
		}
		r.tlsTunnels = append(r.tlsTunnels, tlsTunnel)
	} else if tunnelReq.GetHttp() != nil {
		log.Infof("Received http tunnel request host=%s", tunnelReq.GetHttp().Host)
		protocol = db.HttpProtocol
		url = tunnelReq.GetHttp().GetHost()
		httpsTunnel := &HttpsTunnel{
			host: url,
			sess: sess,
		}
		r.httpsTunnels = append(r.httpsTunnels, httpsTunnel)
	}
	_, err = r.saveSession(initialConn, url, protocol)
	if err != nil {
		return err
	}
	return nil
}
func (r *TunnelRegistry) saveSession(conn net.Conn, url string, protocol db.Protocol) (*db.Tunnel, error) {
	tunnel := &db.Tunnel{
		ID:       uuid.NewString(),
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

type TunnelCtx struct {
	DestConn        net.Conn
	Reader          *bufio.Reader
	Conn            net.Conn
	ClientHelloInfo *tls.ClientHelloInfo
}

func (r *TunnelRegistry) GetTLSSession(clientHello *tls.ClientHelloInfo) (*yamux.Session, error) {
	var sess *yamux.Session
	for _, tunnel := range r.tlsTunnels {
		if exists := tunnel.Match(
			clientHello,
		); exists {
			sess = tunnel.GetSession()
			break
		}
	}
	if sess == nil {
		log.Warnf("Tunnel not found")
		return nil, errors.Errorf("No tunnel found")
	}

	return sess, nil
}

func (r *TunnelRegistry) GetHttpSession(req http.Header) (*yamux.Session, error) {
	var sess *yamux.Session
	for _, tunnel := range r.httpsTunnels {
		if exists := tunnel.Match(
			req,
		); exists {
			sess = tunnel.GetSession()
			break
		}
	}
	if sess == nil {
		log.Warnf("Tunnel not found")
		return nil, errors.Errorf("No tunnel found")
	}

	return sess, nil
}
