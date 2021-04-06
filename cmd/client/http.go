package client

import "github.com/spf13/cobra"

type httpClientCmd struct {
	host    string
	port    int
	tunnel  string
	address string
}

func (h *httpClientCmd) validate() error {
	return nil
}
func (h *httpClientCmd) run() error {
	return nil
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
	cmd.MarkPersistentFlagRequired("port")
	return cmd
}
