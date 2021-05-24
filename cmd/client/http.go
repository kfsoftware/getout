package client

import (
	"github.com/hashicorp/yamux"
	"github.com/kfsoftware/getout/pkg/tunnel"
	log "github.com/schollz/logger"
	"github.com/spf13/cobra"
	"net"
	"time"
)

type httpClientCmd struct {
	host    string
	port    int
	tunnel  string
	address string
}

func (h *httpClientCmd) validate() error {
	return nil
}
func (h *httpClientCmd) startServer() error {
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
	err = tunnelCli.StartHttpTunnel(h.host)
	if err != nil {
		return err
	}
	err = tunnelCli.Start()
	if err != nil {
		return err
	}
	return nil
}
func (h *httpClientCmd) run() error {
	attempts := 0
	retryInterval := 5 * time.Second
	for {
		err := h.startServer()
		if err != nil {
			attempts += 1
			log.Warnf("Error starting the client connection: %v retrying in %s, %d attempts", err, retryInterval, attempts)
			time.Sleep(retryInterval)
		}
	}
}
func newHttpCmd() *cobra.Command {
	c := &httpClientCmd{}
	cmd := &cobra.Command{
		Use: "http",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.validate(); err != nil {
				return err
			}
			return c.run()
		},
	}
	persistentFlags := cmd.PersistentFlags()
	persistentFlags.StringVarP(&c.host, "host", "", "", "Host to match")
	persistentFlags.StringVarP(&c.tunnel, "tunnel", "", "tunnel.arise.kungfusoftware.es:8082", "Tunnel to connect to")
	persistentFlags.StringVarP(&c.address, "address", "", "", "Local address to redirect to")

	cmd.MarkPersistentFlagRequired("host")
	cmd.MarkPersistentFlagRequired("address")
	return cmd
}
