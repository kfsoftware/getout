package server

import (
	"crypto/tls"
	"errors"
	"github.com/kfsoftware/getout/pkg/db"
	"github.com/kfsoftware/getout/pkg/registry"
	"github.com/kfsoftware/getout/pkg/tunnel"
	"github.com/spf13/cobra"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"net"
	"os"
)

type serverCmd struct {
	tunnelAddr    string
	addr          string
	tlsCrt        string
	tlsKey        string
	defaultDomain string
	adminAddr     string
	postgresUrl   string
	db            string
}
type DbType string
const (
	PostgresDbType = "postgres"
	InMemDbType = "memory"
)
func (c *serverCmd) validate() error {
	if c.db != PostgresDbType && c.db != InMemDbType {
		return errors.New("database type not supported")
	}
	return nil
}
func getDb(datasourceName string) *gorm.DB {
	os.Setenv("TZ", "UTC")
	gormConfig := &gorm.Config{}
	dbClient, err := gorm.Open(
		postgres.New(
			postgres.Config{
				DSN:                  datasourceName,
				PreferSimpleProtocol: true,
			},
		),
		gormConfig,
	)
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
	var tunnelRegistry registry.TunnelRegistry
	if c.db == PostgresDbType {
		clientDb := getDb(c.postgresUrl)
		tunnelRegistry = registry.NewPostgresTunnelRegistry(clientDb)
	} else {
		tunnelRegistry = registry.NewInMemoryTunnelRegistry()
	}
	var crts []tls.Certificate
	if c.tlsCrt != "" && c.tlsKey != "" {
		crt, err := tls.LoadX509KeyPair(c.tlsCrt, c.tlsKey)
		if err != nil {
			return err
		}
		crts = append(crts, crt)
	}
	serverListener, err := net.Listen("tcp", c.addr)
	if err != nil {
		return err
	}
	tunnelListener, err := net.Listen("tcp", c.tunnelAddr)
	if err != nil {
		return err
	}
	adminListener, err := net.Listen("tcp", c.adminAddr)
	if err != nil {
		return err
	}
	i := tunnel.NewTunnelServerInstance(
		tunnelRegistry,
		tunnelListener,
		serverListener,
		adminListener,
		c.defaultDomain,
		crts,
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
	persistentFlags.StringVarP(&c.adminAddr, "admin-addr", "", "", "Address to view information")
	persistentFlags.StringVarP(&c.tlsKey, "tls-key", "", "", "Path to a TLS key file")
	persistentFlags.StringVarP(&c.tlsCrt, "tls-crt", "", "", "Path to a TLS certificate file")
	persistentFlags.StringVarP(&c.defaultDomain, "default-domain", "", "", "Default domain where the tunnels are hosted")
	persistentFlags.StringVarP(&c.postgresUrl, "postgres", "", "", "Postgres connection string")
	persistentFlags.StringVarP(&c.db, "db", "", "", "Type of db")

	cmd.MarkPersistentFlagRequired("addr")
	cmd.MarkPersistentFlagRequired("tunnel-addr")
	cmd.MarkPersistentFlagRequired("admin-addr")
	return cmd
}
