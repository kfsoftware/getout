package tunnel

import (
	"github.com/hashicorp/yamux"
	"github.com/kfsoftware/getout/pkg/messages"
	"github.com/kfsoftware/getout/proxy"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"net"
	"time"
)

func NewTunnelClient(tunnelAddress string) *tunnelClient {
	return &tunnelClient{
		tunnelAddress: tunnelAddress,
	}
}

type tunnelClient struct {
	tunnelAddress string
}

func (c *tunnelClient) StartTlsTunnel(sni string, remoteAddress string) error {
	log.Debug().Msgf("Starting TLS tunnel to %s", c.tunnelAddress)
	conn, err := net.Dial("tcp", c.tunnelAddress)
	if err != nil {
		log.Trace().Msgf("Failed to connect to tunnel: %v", err)
		return err
	}
	cfg := yamux.DefaultConfig()
	session, err := yamux.Client(conn, cfg)
	if err != nil {
		log.Trace().Msgf("Failed to create yamux session: %v", err)
		return err
	}
	tunnelReq := &messages.TunnelRequest{
		Req: &messages.TunnelRequest_Tls{
			Tls: &messages.TlsTunnelRequest{
				Sni: sni,
			},
		},
	}
	initialConn, err := session.Open()
	if err != nil {
		return err
	}
	err = messages.WriteMsg(initialConn, tunnelReq)
	if err != nil {
		return err
	}
	tunnelResponse := &messages.TunnelResponse{}
	err = messages.ReadMsgInto(initialConn, tunnelResponse)
	if err != nil {
		log.Trace().Msgf("Failed to read tunnel response: %v", err)
		return err
	}
	switch tunnelResponse.Status {
	case messages.TunnelStatus_ALREADY_EXISTS:
		return errors.Errorf("Tunnel already exists for SNI: %s", sni)
	case messages.TunnelStatus_ERROR:
		return errors.Errorf("error stablishing connection with SNI: %s", sni)
	case messages.TunnelStatus_OK:
		// OK
	default:
		return errors.Errorf("unknown tunnel status: %s", tunnelResponse.Status)
	}
	log.Debug().Msgf("Established initial connection: %v", tunnelResponse)
	err = initialConn.Close()
	if err != nil {
		return err
	}
	err = c.startSNIProxy(session, remoteAddress)
	if err != nil {
		return err
	}
	return nil
}

func (c tunnelClient) startSNIProxy(session *yamux.Session, remoteAddress string) error {
	log.Debug().Msgf("Starting SNI proxy for %s", remoteAddress)
	for {
		conn, err := session.Accept()
		if err != nil {
			log.Trace().Msgf("Failed to accept connections: %v", err)
			return err
		}
		destConn, err := net.DialTimeout("tcp", remoteAddress, time.Second*5)
		if err != nil {
			log.Trace().Msgf("Failed to connect to remote address: %v", err)
			connCloseErr := conn.Close()
			if connCloseErr != nil {
				log.Trace().Msgf("Failed to close connection: %v", connCloseErr)
			}
			if destConn != nil {
				destConn.Close()
			}
			continue
		}
		p := proxy.New(
			conn,
			destConn,
		)
		p.Start()
	}
}
