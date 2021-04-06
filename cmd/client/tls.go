package client

import "github.com/spf13/cobra"

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
