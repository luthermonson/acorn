package cli

import (
	cli "github.com/acorn-io/acorn/pkg/cli/builder"
	"github.com/acorn-io/acorn/pkg/server"
	"github.com/spf13/cobra"
)

var (
	apiServer = server.New()
)

func NewApiServer() *cobra.Command {
	api := &APIServer{}
	cmd := cli.Command(api, cobra.Command{
		Use:          "api-server [flags] [APP_NAME...]",
		SilenceUsage: true,
		Short:        "Run api-server",
		Hidden:       true,
	})
	apiServer.AddFlags(cmd.Flags())
	return cmd
}

type APIServer struct {
	DSN string `usage:"DB DSN" env:"DB_DSN"`
}

func (a *APIServer) Run(cmd *cobra.Command, args []string) error {
	cfg, err := apiServer.NewConfig(cmd.Version)
	if err != nil {
		return err
	}
	cfg.DSN = a.DSN

	return apiServer.Run(cmd.Context(), cfg)
}
