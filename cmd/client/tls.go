package client

import (
	"github.com/hashicorp/yamux"
	"github.com/kfsoftware/getout/pkg/tunnel"
	"github.com/spf13/cobra"
	"net"
)

type tlsClientCmd struct {
	host    string
	port    int
	tunnel  string
	address string
}

func (h *tlsClientCmd) validate() error {
	return nil
}
func (h *tlsClientCmd) run() error {
	conn, err := net.Dial("tcp", h.tunnel)
	if err != nil {
		panic(err)
	}
	session, err := yamux.Client(conn, nil)
	if err != nil {
		panic(err)
	}
	tunnelCli := tunnel.NewTunnelClient(
		session,
		h.address,
	)
	err = tunnelCli.StartTlsTunnel(h.host)
	if err != nil {
		return err
	}
	err = tunnelCli.Start()
	if err != nil {
		return err
	}
	return nil
}
func newTlsCmd() *cobra.Command {
	c := &tlsClientCmd{}
	cmd := &cobra.Command{
		Use: "tls",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.validate(); err != nil {
				return err
			}
			return c.run()
		},
	}
	persistentFlags := cmd.PersistentFlags()
	persistentFlags.StringVarP(&c.host, "host", "", "", "Host to redirect to")
	persistentFlags.StringVarP(&c.tunnel, "tunnel", "", "tunnel.arise.kungfusoftware.es:8082", "Tunnel to connect to")
	persistentFlags.StringVarP(&c.address, "address", "", "localhost", "Local address to redirect to")

	cmd.MarkPersistentFlagRequired("host")
	cmd.MarkPersistentFlagRequired("port")
	return cmd
}
