package cmd

import (
	"github.com/kfsoftware/getout/cmd/client"
	"github.com/kfsoftware/getout/cmd/server"
	"github.com/spf13/cobra"
)

const (
	getOutDesc = `
getout exposes local networked services behinds NATs and firewalls to the
public internet over a secure tunnel. Share local websites, build/test
webhook consumers and self-host personal services.
Detailed help for each command is available with 'getout help <command>'.
`
)

func NewCmdGetOut() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "getout",
		Short: "tunnel local ports to public URLs",
		Long:  getOutDesc,
	}
	cmd.AddCommand(client.NewClientCmd())
	cmd.AddCommand(server.NewServerCmd())

	return cmd
}
