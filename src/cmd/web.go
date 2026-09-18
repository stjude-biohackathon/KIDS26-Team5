package cmd

import (
	"context"

	appmod "antelope/internal/app"
	"antelope/internal/modules/llmconfig"
	"antelope/internal/modules/log"
	"antelope/internal/modules/storage"
	"antelope/models"

	"github.com/redis/go-redis/v9"
	"github.com/urfave/cli/v3"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// CmdWeb starts the HTTP API server together with the Nomad event monitor
// in the same process (two goroutines).  This is the recommended production mode.
var CmdWeb = &cli.Command{
	Name:  "web",
	Usage: "Start the HTTP API server (+ Nomad event monitor goroutine)",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "config",
			Aliases: []string{"c"},
			Usage:   "Path to the configuration file",
		},
		&cli.IntFlag{
			Name:    "port",
			Aliases: []string{"p"},
			Usage:   "Override the HTTP listen port",
		},
	},
	Action: func(ctx context.Context, cmd *cli.Command) error {
		cfg, err := setupConfig(cmd.String("config"))
		if err != nil {
			return err
		}

		// Allow CLI flag to override the configured port.
		if p := cmd.Int("port"); p != 0 {
			cfg.System.Port = p
		}

		a := appmod.NewApp(cfg,
			// Migrate the domain models and the infra-layer secret tables together
			// under the same distributed lock (see app.WithMigration).
			appmod.WithMigration(func(db *gorm.DB) error {
				if err := models.Migrate(db); err != nil {
					return err
				}
				if err := storage.MigrateSchema(db); err != nil {
					return err
				}
				// Runs inside the migration lock so a multi-pod rollout does
				// not race to copy the same legacy rows.
				if err := storage.BackfillPersonalConfigs(db); err != nil {
					return err
				}
				return llmconfig.MigrateSchema(db)
			}),
			appmod.WithSeed(func(db *gorm.DB, _ redis.UniversalClient, sm *storage.ClientManager) {
				models.Seed(db, models.SeedConfig{
					SuperUser:         cfg.System.SuperUser,
					SuperUserPassword: cfg.System.SuperUserPassword,
				})
				// Needs the manager's encryption key, so it cannot run in the
				// migration step above.
				if sm != nil {
					if err := sm.BackfillEndpoints(); err != nil {
						log.L().Error("failed to backfill storage endpoints", zap.Error(err))
					}
				}
			}),
		)
		defer a.Shutdown()

		srv := NewServer(a)
		srv.RunWeb()
		return nil
	},
}
