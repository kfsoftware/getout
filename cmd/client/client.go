package client

import (
	"fmt"
	"time"

	"github.com/kfsoftware/getout/pkg/tunnel"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

type clientCmd struct {
	sni    string
	port   int
	tunnel string
	host   string
}

func (c *clientCmd) validate() error {
	return nil
}
func (c *clientCmd) startTunnel() error {
	tunnelClient := tunnel.NewTunnelClient(c.tunnel)
	remoteAddress := fmt.Sprintf("%s:%d", c.host, c.port)
	err := tunnelClient.StartTlsTunnel(c.sni, remoteAddress)
	if err != nil {
		log.Trace().Msgf("Failed to start tls tunnel: %v", err)
		return err
	}
	log.Info().Msgf("Connection established, waiting for connections..")
	return nil
}
func (c *clientCmd) run() error {
	for {
		log.Info().Msgf("Connecting to %s:%d", c.host, c.port)
		err := c.startTunnel()
		if err != nil {
			log.Error().Msgf("Failed to start tunnel: %v retrying in %v", err, 5*time.Second)
			time.Sleep(5 * time.Second)
			continue
		} else {
			log.Info().Msgf("Tunnel started")
			break
		}
	}
	return nil
}
func NewClientCmd() *cobra.Command {
	c := &clientCmd{}
	cmd := &cobra.Command{
		Use: "client",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.validate(); err != nil {
				return err
			}
			return c.run()
		},
	}
	persistentFlags := cmd.PersistentFlags()
	persistentFlags.StringVarP(&c.sni, "sni", "", "", "SNI Host to listen for")
	persistentFlags.StringVarP(&c.tunnel, "tunnel", "", "tunnel.arise.kungfusoftware.es:8082", "Tunnel to connect to")
	persistentFlags.IntVarP(&c.port, "port", "", 0, "Local port to redirect to")
	persistentFlags.StringVarP(&c.host, "host", "", "localhost", "Local host to redirect to")

	cmd.MarkPersistentFlagRequired("sni")
	cmd.MarkPersistentFlagRequired("port")
	return cmd
}
