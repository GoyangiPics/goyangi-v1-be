package bot

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/disgoorg/disgo"
	disbot "github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/gateway"
	"github.com/pocketbase/pocketbase"
)

// App is the PocketBase app instance, injected by Start. Used by the record
// helpers (records.go) and the slash commands (commands.go).
var App *pocketbase.PocketBase

// Client is the disgo client, stored on Start so goroutines outside the event
// handlers (e.g. the AVIF-ready notifier) can call the Discord REST API.
var Client *disbot.Client

// Start connects the Discord bot and registers its handlers and slash
// commands. The PocketBase app is stored for internal database operations.
func Start(pbApp *pocketbase.PocketBase) error {
	App = pbApp

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

	if err := client.OpenGateway(context.Background()); err != nil {
		return fmt.Errorf("open discord gateway: %w", err)
	}

	registerSlashCommands(client)

	slog.Info("Discord bot is now running")
	return nil
}
