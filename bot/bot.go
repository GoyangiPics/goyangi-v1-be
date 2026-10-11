package bot

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/disgoorg/disgo"
	disbot "github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/gateway"
	"github.com/pocketbase/pocketbase/core"
)

// App is the PocketBase app instance, injected by Init. Used by the record
// helpers (records.go), the slash commands (commands.go) and the ingest API.
var App core.App

// Init hands the package the PocketBase app. Called at boot whether or not the
// gateway bot runs: the ingest API creates records through the same helpers.
func Init(app core.App) {
	App = app
	// Known even when the bot doesn't connect, so a mention of it still reads
	// as a mention of goyangi (see isGoyangiBot).
	if id := botIDFromToken(os.Getenv("DISCORD_TOKEN")); id != 0 {
		ourBotID.Store(uint64(id))
	}
	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		if err := ensureDiscordLinkIndex(e.App); err != nil {
			slog.Warn("could not ensure the contents.discord index", "err", err)
		}
		return nil
	})
}

// discordLinkIndex makes "is this message already in the archive?" a lookup
// rather than a scan of every post: alreadyIngested asks it for every message
// a trigger fires on, through either door.
const (
	discordLinkIndexName = "idx_contents_discord"
	discordLinkIndex     = "CREATE INDEX `" + discordLinkIndexName + "` ON `contents` (`discord`)"
)

// ensureDiscordLinkIndex adds the index if no index of that name exists.
// Idempotent; the same approach as hooks/uploader_stats.go.
func ensureDiscordLinkIndex(app core.App) error {
	collection, err := app.FindCollectionByNameOrId("contents")
	if err != nil {
		return err
	}
	for _, idx := range collection.Indexes {
		if strings.Contains(idx, "`"+discordLinkIndexName+"`") {
			return nil
		}
	}
	collection.Indexes = append(collection.Indexes, discordLinkIndex)
	return app.Save(collection)
}

// Enabled reports whether this process should connect its own Discord bot.
//
// DISCORD_BOT_ENABLED=false runs the backend without one — for when another
// bot reads the channels and forwards messages over the ingest API instead
// (ingest_api.go). Unset means enabled, so existing deploys are unchanged, and
// turning it back on restores everything: slash commands, the context menu,
// passive ingestion, AVIF notices and upload announcements. The announcers
// already do nothing without a client (notify.go, autopost.go).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DISCORD_BOT_ENABLED"))) {
	case "false", "0", "off", "no":
		return false
	default:
		return true
	}
}

// Client is the disgo client, stored on Start so goroutines outside the event
// handlers (e.g. the AVIF-ready notifier) can call the Discord REST API.
var Client *disbot.Client

// Start connects the Discord bot and registers its handlers and slash
// commands. Init must have run first.
func Start() error {
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		return fmt.Errorf("DISCORD_TOKEN is required")
	}

	client, err := disgo.New(token,
		// Exactly the three intents the handlers consume — Discord's intent
		// review scores "request only what you need", and nothing here
		// subscribes to bans, invites, voice, typing, reactions or DMs:
		//
		//   - Guilds: gateway session bookkeeping (required for guild events).
		//   - GuildMessages: MessageCreate, the only gateway event handled.
		//     Reactions the bot ADDS are REST calls, not a gateway
		//     subscription, and interactions (slash/context/modal/buttons)
		//     arrive regardless of intents.
		//   - MessageContent: privileged, and NOT part of the non-privileged
		//     set. Without it Discord delivers guild messages with empty
		//     content and empty attachments (except messages that @-mention
		//     the bot), which silently kills the role-ping, reply, follow-up
		//     and text-detection ingestion paths. It must ALSO be enabled for
		//     the application under Bot → Privileged Gateway Intents in the
		//     Discord developer portal, or the gateway rejects the connection.
		disbot.WithGatewayConfigOpts(
			gateway.WithIntents(
				gateway.IntentGuilds|gateway.IntentGuildMessages|gateway.IntentMessageContent,
			),
		),
		// tracked: counted in Busy() so a supervised restart waits for them.
		disbot.WithEventListenerFunc(tracked(onMessageCreate)),
		disbot.WithEventListenerFunc(tracked(onCommand)),
		disbot.WithEventListenerFunc(tracked(onComponent)),
		disbot.WithEventListenerFunc(tracked(onAutocomplete)),
		disbot.WithEventListenerFunc(tracked(onModalSubmit)),
	)
	if err != nil {
		return fmt.Errorf("create discord client: %w", err)
	}

	Client = client
	ourBotID.Store(uint64(client.ApplicationID))

	if err := client.OpenGateway(context.Background()); err != nil {
		return fmt.Errorf("open discord gateway: %w", err)
	}

	registerSlashCommands(client)

	slog.Info("Discord bot is now running")
	return nil
}
