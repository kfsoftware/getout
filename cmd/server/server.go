package server

import (
	"crypto/tls"
	"github.com/kfsoftware/getout/pkg/db"
	"github.com/kfsoftware/getout/pkg/registry"
	"github.com/kfsoftware/getout/pkg/tunnel"
	"github.com/spf13/cobra"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"os"
)

type serverCmd struct {
	tunnelAddr    string
	addr          string
	tlsCrt        string
	tlsKey        string
	defaultDomain string
}

func (c *serverCmd) validate() error {
	return nil
}
func getDb() *gorm.DB {
	os.Setenv("TZ", "UTC")

	dbClient, err := gorm.Open(sqlite.Open("/disco-grande/go/src/github.com/kfsoftware/getout/getout.db"), &gorm.Config{

	})
	if err != nil {
		panic(err)
	}
	err = dbClient.AutoMigrate(&db.Tunnel{})
	if err != nil {
		panic(err)
	}
	return dbClient
}
func (c *serverCmd) run() error {
	clientDb := getDb()
	tunnelRegistry := registry.NewTunnelRegistry(clientDb)
	crt, err := tls.LoadX509KeyPair(c.tlsCrt, c.tlsKey)
	if err != nil {
		return err
	}
	i := tunnel.NewTunnelServerInstance(
		tunnelRegistry,
		c.tunnelAddr,
		c.addr,
		c.defaultDomain,
		[]tls.Certificate{crt},
	)
	err = i.Start()
	if err != nil {
		return err
	}
	return nil
}

func NewServerCmd() *cobra.Command {
	c := &serverCmd{}
	cmd := &cobra.Command{
		Use: "server",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.validate(); err != nil {
				return err
			}
			return c.run()
		},
	}
	persistentFlags := cmd.PersistentFlags()
	persistentFlags.StringVarP(&c.addr, "addr", "", "", "Address to listen for requests")
	persistentFlags.StringVarP(&c.tunnelAddr, "tunnel-addr", "", "", "Address to manage the tunnel connections")
	persistentFlags.StringVarP(&c.tlsKey, "tls-key", "", "", "Path to a TLS key file")
	persistentFlags.StringVarP(&c.tlsCrt, "tls-crt", "", "", "Path to a TLS certificate file")
	persistentFlags.StringVarP(&c.defaultDomain, "default-domain", "", "", "Default domain where the tunnels are hosted")

	cmd.MarkPersistentFlagRequired("addr")
	cmd.MarkPersistentFlagRequired("tunnel-addr")
	cmd.MarkPersistentFlagRequired("tls-key")
	cmd.MarkPersistentFlagRequired("tls-crt")
	cmd.MarkPersistentFlagRequired("default-domain")
	return cmd
}
