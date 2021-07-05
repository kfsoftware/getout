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

type TunnelRegistry interface {
	RemoveSession(id string) error
	StoreSession(
		sess *yamux.Session,
	) (ITunnel, error)
	GetTLSSession(clientHello *tls.ClientHelloInfo) (*yamux.Session, error)
	GetHttpTunnels() (map[string]IHttpsTunnel, error)
	GetTLSTunnels() (map[string]ITlsTunnel, error)
	GetHttpSession(req http.Header) (*yamux.Session, error)
}
type InMemTunnelRegistry struct {
	tlsTunnels   map[string]ITlsTunnel
	httpsTunnels map[string]IHttpsTunnel
}

func (i InMemTunnelRegistry) RemoveSession(id string) error {
	tlsTunnels, err := i.GetTLSTunnels()
	if err != nil {
		return err
	}
	_, ok := tlsTunnels[id]
	if ok {
		i.removeTlsTunnel(id)
		return nil
	}
	httpsTunnels, err := i.GetHttpTunnels()
	if err != nil {
		return err
	}
	_, ok = httpsTunnels[id]
	if ok {
		i.removeHttpTunnel(id)
		return nil
	}
	return nil
}
func (i *InMemTunnelRegistry) removeHttpTunnel(id string) {
	delete(i.httpsTunnels, id)
}
func (i *InMemTunnelRegistry) removeTlsTunnel(id string) {
	delete(i.tlsTunnels, id)
}
func NewTlsTunnel() ITlsTunnel {
	return &InMemTlsTunnel{}
}
func NewHttpsTunnel() IHttpsTunnel {
	return &InMemHttpsTunnel{}
}
func (i *InMemTunnelRegistry) GetHttpTunnels() (map[string]IHttpsTunnel, error) {
	return i.httpsTunnels, nil
}
func (i *InMemTunnelRegistry) GetTLSTunnels() (map[string]ITlsTunnel, error) {
	return i.tlsTunnels, nil
}
func (i *InMemTunnelRegistry) GetHttpSession(req http.Header) (*yamux.Session, error) {
	var sess *yamux.Session
	httpTunnels, err := i.GetHttpTunnels()
	if err != nil {
		return nil, err
	}
	for _, httpsTunnel := range httpTunnels {
		if exists := httpsTunnel.Match(
			req,
		); exists {
			sess = httpsTunnel.GetSession()
			break
		}
	}
	if sess == nil {
		return nil, errors.Errorf("No httpsTunnel found")
	}
	return sess, nil
}

func (i InMemTunnelRegistry) StoreSession(sess *yamux.Session) (ITunnel, error) {
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
	if tunnelReq.GetTls() != nil {
		url = tunnelReq.GetTls().GetSni()
		log.Infof("Received tls tunnel request %s", url)
	} else if tunnelReq.GetHttp() != nil {
		log.Infof("Received http tunnel request host=%s", tunnelReq.GetHttp().Host)
		url = tunnelReq.GetHttp().GetHost()
	}
	log.Tracef("Saving session")
	tunnelID := uuid.NewString()
	var tunn ITunnel
	if tunnelReq.GetTls() != nil {
		url = tunnelReq.GetTls().GetSni()
		tlsTunnel := InMemTlsTunnel{
			sni:  url,
			sess: sess,
			id:   tunnelID,
		}
		tunn = tlsTunnel
		i.tlsTunnels[tunnelID] = tlsTunnel
	} else if tunnelReq.GetHttp() != nil {
		log.Infof("Received http tunnel request host=%s", tunnelReq.GetHttp().Host)
		url = tunnelReq.GetHttp().GetHost()
		httpsTunnel := InMemHttpsTunnel{
			host: url,
			sess: sess,
			id:   tunnelID,
		}
		tunn = httpsTunnel
		i.httpsTunnels[tunnelID] = httpsTunnel
	}

	return tunn, nil
}

func (i InMemTunnelRegistry) GetTLSSession(clientHello *tls.ClientHelloInfo) (*yamux.Session, error) {
	var sess *yamux.Session
	for _, tlsTunnel := range i.tlsTunnels {
		if exists := tlsTunnel.Match(
			clientHello,
		); exists {
			sess = tlsTunnel.GetSession()
			break
		}
	}
	if sess == nil {
		return nil, errors.Errorf("No tlsTunnel found")
	}

	return sess, nil
}

type PostgresTunnelRegistry struct {
	tlsTunnels   map[string]ITlsTunnel
	httpsTunnels map[string]IHttpsTunnel
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
type IHttpsTunnel interface {
	Match(req http.Header) bool
	GetSession() *yamux.Session
	GetID() string
	GetProperties() (HttpTunnelProperties, error)
}
type ITunnel interface {
	GetSession() *yamux.Session
	GetID() string
}
type ITlsTunnel interface {
	Match(clientHello *tls.ClientHelloInfo) bool
	GetSession() *yamux.Session
	GetID() string
	GetProperties() (TlsTunnelProperties, error)
}
type HttpTunnelProperties struct {
	Host          string
	RemoteAddress string
}
type TlsTunnelProperties struct {
	SNI           string
	RemoteAddress string
}
type InMemHttpsTunnel struct {
	host string
	sess *yamux.Session
	id   string
}

func (t InMemHttpsTunnel) GetProperties() (HttpTunnelProperties, error) {
	return HttpTunnelProperties{
		Host:          t.host,
		RemoteAddress: t.sess.RemoteAddr().String(),
	}, nil
}

func (t InMemHttpsTunnel) GetID() string {
	return t.id
}

func (t InMemHttpsTunnel) Match(req http.Header) bool {
	host := strings.Split(req.Get("Host"), ":")[0]
	return host == t.host
}

func (t InMemHttpsTunnel) GetSession() *yamux.Session {
	return t.sess
}

type InMemTlsTunnel struct {
	tunnel *db.Tunnel
	sni    string
	sess   *yamux.Session
	id     string
}

func (t InMemTlsTunnel) GetProperties() (TlsTunnelProperties, error) {
	return TlsTunnelProperties{
		SNI:           t.sni,
		RemoteAddress: t.sess.RemoteAddr().String(),
	}, nil
}

func (t InMemTlsTunnel) GetID() string {
	return t.id
}

func (t InMemTlsTunnel) Match(clientHello *tls.ClientHelloInfo) bool {
	if clientHello == nil {
		return false
	}
	return clientHello.ServerName == t.sni
}
func (t InMemTlsTunnel) GetSession() *yamux.Session {
	return t.sess
}

type PostgresHttpsTunnel struct {
	tunnel *db.Tunnel
	host   string
	sess   *yamux.Session
	id     string
}

func (t PostgresHttpsTunnel) GetProperties() (HttpTunnelProperties, error) {
	return HttpTunnelProperties{
		Host:          t.host,
		RemoteAddress: t.sess.RemoteAddr().String(),
	}, nil
}

func (t PostgresHttpsTunnel) GetID() string {
	return t.id
}

func (t PostgresHttpsTunnel) Match(req http.Header) bool {
	host := strings.Split(req.Get("Host"), ":")[0]
	return host == t.host
}
func (t PostgresHttpsTunnel) GetSession() *yamux.Session {
	return t.sess
}

type PostgresTlsTunnel struct {
	tunnel *db.Tunnel
	sni    string
	sess   *yamux.Session
	id     string
}

func (t PostgresTlsTunnel) GetProperties() (TlsTunnelProperties, error) {
	return TlsTunnelProperties{
		SNI:           t.sni,
		RemoteAddress: t.sess.RemoteAddr().String(),
	}, nil
}

func (t PostgresTlsTunnel) GetID() string {
	return t.id
}

func (t PostgresTlsTunnel) Match(clientHello *tls.ClientHelloInfo) bool {
	if clientHello == nil {
		return false
	}
	return clientHello.ServerName == t.sni
}
func (t PostgresTlsTunnel) GetSession() *yamux.Session {
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

func NewInMemoryTunnelRegistry() TunnelRegistry {
	return &InMemTunnelRegistry{
		httpsTunnels: map[string]IHttpsTunnel{},
		tlsTunnels:   map[string]ITlsTunnel{},
	}
}
func NewPostgresTunnelRegistry(db *gorm.DB) TunnelRegistry {
	return &PostgresTunnelRegistry{
		httpsTunnels: map[string]IHttpsTunnel{},
		tlsTunnels:   map[string]ITlsTunnel{},
		db:           db,
	}
}
func (r *PostgresTunnelRegistry) RemoveSession(id string) error {
	tunn, err := r.getTunnelById(id)
	if err != nil {
		return err
	}
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
func (r *PostgresTunnelRegistry) StoreSession(
	sess *yamux.Session,
) (ITunnel, error) {
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
	var itunn ITunnel
	if tunnelReq.GetTls() != nil {
		protocol = db.TlsProtocol
		url = tunnelReq.GetTls().GetSni()
		tlsTunnel := &PostgresTlsTunnel{
			sni:    url,
			sess:   sess,
			tunnel: tunn,
		}
		itunn = tlsTunnel
		r.tlsTunnels[tunn.ID] = tlsTunnel
	} else if tunnelReq.GetHttp() != nil {
		log.Infof("Received http tunnel request host=%s", tunnelReq.GetHttp().Host)
		protocol = db.HttpProtocol
		url = tunnelReq.GetHttp().GetHost()
		httpsTunnel := PostgresHttpsTunnel{
			host:   url,
			sess:   sess,
			tunnel: tunn,
		}
		itunn = httpsTunnel
		r.httpsTunnels[tunn.ID] = httpsTunnel
	}

	return itunn, nil
}
func (r *PostgresTunnelRegistry) saveSession(conn net.Conn, url string, protocol db.Protocol) (*db.Tunnel, error) {
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

func (r *PostgresTunnelRegistry) GetTLSSession(clientHello *tls.ClientHelloInfo) (*yamux.Session, error) {
	var sess *yamux.Session
	for _, tlsTunnel := range r.tlsTunnels {
		if exists := tlsTunnel.Match(
			clientHello,
		); exists {
			sess = tlsTunnel.GetSession()
			break
		}
	}
	if sess == nil {
		return nil, errors.Errorf("No tlsTunnel found")
	}

	return sess, nil
}

func (r *PostgresTunnelRegistry) GetHttpTunnels() (map[string]IHttpsTunnel, error) {
	return r.httpsTunnels, nil
}
func (r *PostgresTunnelRegistry) GetTLSTunnels() (map[string]ITlsTunnel, error) {
	return r.tlsTunnels, nil
}
func (r *PostgresTunnelRegistry) GetHttpSession(req http.Header) (*yamux.Session, error) {
	var sess *yamux.Session
	httpTunnels, err := r.GetHttpTunnels()
	if err != nil {
		return nil, err
	}
	for _, httpsTunnel := range httpTunnels {
		if exists := httpsTunnel.Match(
			req,
		); exists {
			sess = httpsTunnel.GetSession()
			break
		}
	}
	if sess == nil {
		return nil, errors.Errorf("No httpsTunnel found")
	}
	return sess, nil
}
func (r *PostgresTunnelRegistry) getTunnelById(id string) (*db.Tunnel, error) {
	var tunn *db.Tunnel
	result := r.db.Find(tunn, id)
	if result.Error != nil {
		return nil, result.Error
	}
	return tunn, nil
}
func (r *PostgresTunnelRegistry) removeHttpTunnel(tunn *db.Tunnel) {
	delete(r.httpsTunnels, tunn.ID)
}
func (r *PostgresTunnelRegistry) removeTlsTunnel(tunn *db.Tunnel) {
	delete(r.tlsTunnels, tunn.ID)
}
