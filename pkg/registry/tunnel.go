package registry

import (
	"encoding/binary"
	"github.com/golang/protobuf/proto"
	"github.com/hashicorp/yamux"
	"github.com/kfsoftware/getout/pkg/messages"
	log "github.com/schollz/logger"
	"net"
)

type TunnelRegistry struct {
	sessions []*Session
}
type Protocol string

const (
	HttpProtocol Protocol = "http"
	TlsProtocol  Protocol = "tls"
)

type Tunnel struct {
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
	sni := make([]byte, sz)
	n, err := initialConn.Read(sni)
	log.Debugf("Read message %s %d", sni, n)
	if err != nil {
		log.Warnf("Failed to read initial connection: %v", err)
		return err
	}
	tunnelReq := &messages.TunnelRequest{}
	err = proto.Unmarshal(sni, tunnelReq)
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
func (r* TunnelRegistry) storeTlsSession(
	sess *yamux.Session,
	props TlsProps,
) {

}
func (r *TunnelRegistry) storeHttpSession(
	sess *yamux.Session,
	props HttpProps,
) {

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
