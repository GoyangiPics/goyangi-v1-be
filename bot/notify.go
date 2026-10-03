package bot

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"goyangi-v1-be/hooks"

	"github.com/disgoorg/disgo/discord"
)

var (
	batchMu    sync.Mutex
	batch      []hooks.AvifInfo
	batchTimer *time.Timer
)

const batchWindow = 20 * time.Second

// QueueAvifNotification adds one AVIF to the pending batch.
// The batch is flushed to Discord batchWindow after the last item arrives.
func QueueAvifNotification(info hooks.AvifInfo) {
	batchMu.Lock()
	defer batchMu.Unlock()

	batch = append(batch, info)
	slog.Info("notify: queued AVIF", "url", info.AvifURL, "pending", len(batch))

	if batchTimer != nil {
		batchTimer.Stop()
	}
	batchTimer = time.AfterFunc(batchWindow, flushBatch)
}

func flushBatch() {
	flushesInFlight.Add(1)
	defer flushesInFlight.Add(-1)

	batchMu.Lock()
	toSend := batch
	batch = nil
	batchTimer = nil
	batchMu.Unlock()

	if len(toSend) == 0 {
		return
	}

	// Falls back to the main bot's notify channel; snowflakeEnv already logs the
	// offending value, so 0 here means the override was set and unparseable.
	channelID := notifyChannelID()
	if channelID == 0 {
		slog.Warn("notify: DISCORD_NOTIFY_CHANNEL_ID is invalid, dropping batch")
		return
	}
	if Client == nil {
		slog.Warn("notify: Discord client is nil")
		return
	}

	msg := buildMessage(toSend)
	slog.Info("notify: flushing batch", "count", len(toSend), "channelID", channelID)
	if _, err := Client.Rest.CreateMessage(channelID, discord.NewMessageCreate().WithContent(msg)); err != nil {
		slog.Error("notify: failed to send message", "err", err)
	}
}

func buildMessage(items []hooks.AvifInfo) string {
	var sb strings.Builder

	// Format-neutral wording: a batch can mix AVIF and WebP previews, and the
	// notification just links the finished preview URL either way.
	if len(items) == 1 {
		sb.WriteString("**✅ Preview ready**\n\n")
	} else {
		fmt.Fprintf(&sb, "**✅ %d previews ready**\n\n", len(items))
	}

	for _, item := range items {
		// Header line: Idol · Group — date
		header := item.Title
		if item.Idol != "" && item.Group != "" {
			header = fmt.Sprintf("**%s · %s**", item.Idol, item.Group)
		} else if item.Title != "" {
			header = fmt.Sprintf("**%s**", item.Title)
		}
		if item.Date != "" {
			header += " — " + formatDate(item.Date)
		}
		sb.WriteString(header + "\n")

		// Uploader + source
		meta := []string{}
		if item.Uploader != "" {
			meta = append(meta, "👤 "+item.Uploader)
		}
		if item.Source != "" {
			meta = append(meta, "🔗 "+item.Source)
		}
		if len(meta) > 0 {
			sb.WriteString(strings.Join(meta, "  ") + "\n")
		}

		sb.WriteString("`" + item.AvifURL + "`\n\n")
	}

	return strings.TrimRight(sb.String(), "\n")
}

// formatDate converts YYMMDD → YY-MM-DD for readability.
func formatDate(d string) string {
	if len(d) != 6 {
		return d
	}
	return d[0:2] + "-" + d[2:4] + "-" + d[4:6]
}
