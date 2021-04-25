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
	"time"
)

type TunnelRegistry struct {
	tlsTunnels   map[string]*TlsTunnel
	httpsTunnels map[string]*HttpsTunnel
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
	tunnel *db.Tunnel
	host   string
	sess   *yamux.Session
}

func (t *HttpsTunnel) Match(req http.Header) bool {
	host := strings.Split(req.Get("Host"), ":")[0]
	return host == t.host
}
func (t *HttpsTunnel) GetSession() *yamux.Session {
	return t.sess
}

type TlsTunnel struct {
	tunnel *db.Tunnel
	sni    string
	sess   *yamux.Session
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
		httpsTunnels: map[string]*HttpsTunnel{},
		tlsTunnels:   map[string]*TlsTunnel{},
		db:           db,
	}
}
func (r *TunnelRegistry) RemoveSession(tunn *db.Tunnel) error {
	tunn.Active = false
	tunn.DeletedAt = gorm.DeletedAt{
		Time:  time.Now(),
		Valid: true,
	}
	result := r.db.Save(tunn)
	if result.Error != nil {
		return result.Error
	}
	switch tunn.Protocol {
	case db.TlsProtocol:
		r.removeTlsTunnel(tunn)
	case db.HttpProtocol:
		r.removeHttpTunnel(tunn)
	}
	return nil
}
func (r *TunnelRegistry) StoreSession(
	sess *yamux.Session,
) (*db.Tunnel, error) {
	initialConn, err := sess.Accept()
	if err != nil {
		log.Warnf("Failed to accept connection: %v", err)
		return nil, err
	}
	defer initialConn.Close()
	var sz int64
	err = binary.Read(initialConn, binary.LittleEndian, &sz)
	reqBytes := make([]byte, sz)
	_, err = initialConn.Read(reqBytes)
	if err != nil {
		log.Warnf("Failed to read initial connection: %v", err)
		return nil, err
	}
	tunnelReq := &messages.TunnelRequest{}
	err = proto.Unmarshal(reqBytes, tunnelReq)
	if err != nil {
		return nil, err
	}
	var url string
	var protocol db.Protocol
	if tunnelReq.GetTls() != nil {
		protocol = db.TlsProtocol
		url = tunnelReq.GetTls().GetSni()
		log.Infof("Received tls tunnel request %s", url)
	} else if tunnelReq.GetHttp() != nil {
		log.Infof("Received http tunnel request host=%s", tunnelReq.GetHttp().Host)
		protocol = db.HttpProtocol
		url = tunnelReq.GetHttp().GetHost()
	}
	log.Tracef("Saving session")
	tunn, err := r.saveSession(initialConn, url, protocol)
	if err != nil {
		return nil, err
	}

	if tunnelReq.GetTls() != nil {
		protocol = db.TlsProtocol
		url = tunnelReq.GetTls().GetSni()
		tlsTunnel := &TlsTunnel{
			sni:    url,
			sess:   sess,
			tunnel: tunn,
		}
		r.tlsTunnels[tunn.ID] = tlsTunnel
	} else if tunnelReq.GetHttp() != nil {
		log.Infof("Received http tunnel request host=%s", tunnelReq.GetHttp().Host)
		protocol = db.HttpProtocol
		url = tunnelReq.GetHttp().GetHost()
		httpsTunnel := &HttpsTunnel{
			host:   url,
			sess:   sess,
			tunnel: tunn,
		}
		r.httpsTunnels[tunn.ID] = httpsTunnel
	}

	return tunn, nil
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

func (r *TunnelRegistry) GetTLSSession(clientHello *tls.ClientHelloInfo) (*yamux.Session, *db.Tunnel, error) {
	var sess *yamux.Session
	var tunn *db.Tunnel
	for _, tlsTunnel := range r.tlsTunnels {
		if exists := tlsTunnel.Match(
			clientHello,
		); exists {
			tunn = tlsTunnel.tunnel
			sess = tlsTunnel.GetSession()
			break
		}
	}
	if sess == nil {
		return nil, nil, errors.Errorf("No tlsTunnel found")
	}

	return sess, tunn, nil
}

func (r *TunnelRegistry) GetHttpTunnels() (map[string]*HttpsTunnel, error) {
	return r.httpsTunnels, nil
}
func (r *TunnelRegistry) GetTLSTunnels() (map[string]*TlsTunnel, error) {
	return r.tlsTunnels, nil
}
func (r *TunnelRegistry) GetHttpSession(req http.Header) (*yamux.Session, *db.Tunnel, error) {
	var sess *yamux.Session
	var tunn *db.Tunnel
	for _, httpsTunnel := range r.httpsTunnels {
		if exists := httpsTunnel.Match(
			req,
		); exists {
			tunn = httpsTunnel.tunnel
			sess = httpsTunnel.GetSession()
			break
		}
	}
	if sess == nil {
		return nil, nil, errors.Errorf("No httpsTunnel found")
	}

	return sess, tunn, nil
}

func (r *TunnelRegistry) removeHttpTunnel(tunn *db.Tunnel) {
	delete(r.httpsTunnels, tunn.ID)
}
func (r *TunnelRegistry) removeTlsTunnel(tunn *db.Tunnel) {
	delete(r.tlsTunnels, tunn.ID)
}
