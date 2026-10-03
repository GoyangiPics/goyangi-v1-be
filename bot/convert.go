package bot

import (
	"bytes"
	"net/url"
	"path"
	"strings"

	"goyangi-v1-be/hooks"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
)

// maxDiscordUpload is the attachment ceiling for bot uploads in non-boosted
// guilds. Larger outputs are reported instead of failing the upload.
const maxDiscordUpload = 10 << 20 // 10 MiB

// /convert and /revive both run the stateless encode pipeline — the
// /api/convert flow exposed in Discord. Nothing is persisted and no record is
// created; the output comes back as an attachment.
//
// /revive is the narrow case: an imgur link whose file is gone from our
// library (or was never in it), re-encoded to AVIF. /convert is the general
// form, taking an attachment or any link and any output format.

// handleConvertCommand converts an attachment or link to the chosen format.
func handleConvertCommand(e *events.ApplicationCommandInteractionCreate, data discord.SlashCommandInteractionData) {
	r := newReply(e)

	var srcURL, srcName string
	if attachment, ok := data.OptAttachment("file"); ok {
		srcURL = attachment.URL
		srcName = attachment.Filename
	} else if link, ok := data.OptString("link"); ok {
		srcURL = strings.TrimSpace(link)
		srcName = nameFromURL(srcURL)
	} else {
		r.Fail("Attach a `file` or pass a `link` to convert.")
		return
	}

	format := hooks.ConvertKind(optString(data, "format", string(hooks.ConvertAVIF)))

	if !r.Defer() {
		return
	}
	runEncode(r, srcURL, srcName, format)
}

// handleReviveCommand re-encodes the file behind an imgur link through the
// AVIF pipeline. Unlike /match it never touches the library — the point is to
// get a playable file back out of a link, whether or not we ever stored it.
func handleReviveCommand(e *events.ApplicationCommandInteractionCreate, data discord.SlashCommandInteractionData) {
	link := normalizeImgurOption(strings.TrimSpace(data.String("link")))

	r := newReply(e)
	if !r.Defer() {
		return
	}
	runEncode(r, link, nameFromURL(link), hooks.ConvertAVIF)
}

// runEncode downloads the source, pushes it through one of the encode
// pipelines and posts the result as an attachment. Every failure stays with
// the invoking user — a dead link or a busy encoder is not channel news.
func runEncode(r *reply, srcURL, srcName string, format hooks.ConvertKind) {
	if srcURL == "" {
		r.Fail("No source to convert.")
		return
	}

	content, contentType, suggestedName, err := downloadFile(srcURL)
	if err != nil {
		r.Fail("Download failed: %v", err)
		return
	}
	if suggestedName != "" {
		srcName = suggestedName
	}

	// The encode pipeline picks its ffmpeg input format from the extension.
	srcExt := path.Ext(srcName)
	if srcExt == "" {
		srcExt = extFromContentType(contentType)
	}
	if srcExt == "" {
		srcExt = ".mp4"
	}

	out, outExt, err := hooks.Convert(format, content, srcExt)
	if err != nil {
		r.Fail("Conversion failed: %v", err)
		return
	}
	if len(out) > maxDiscordUpload {
		r.Fail("Output is %.1f MB — too large to attach here.", float64(len(out))/(1024*1024))
		return
	}

	outName := strings.TrimSuffix(srcName, srcExt)
	if outName == "" {
		outName = "converted"
	}
	r.Publish(discord.NewMessageCreate().
		WithContentf("✅ `%s` → **%s** (%.1f MB → %.1f MB)",
			srcName, format, float64(len(content))/(1024*1024), float64(len(out))/(1024*1024)).
		AddFile(outName+outExt, "", bytes.NewReader(out)))
}

// nameFromURL derives a filename from a link's last path segment.
func nameFromURL(link string) string {
	parsed, err := url.Parse(link)
	if err != nil {
		return ""
	}
	name := path.Base(parsed.Path)
	if name == "." || name == "/" {
		return ""
	}
	return name
}

// reviveCommandCreate is the /revive slash command definition.
func reviveCommandCreate() discord.SlashCommandCreate {
	return discord.SlashCommandCreate{
		Name:        "revive",
		Description: "Re-encode an imgur link through the goyangi AVIF pipeline (nothing is saved).",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{
				Name:        "link",
				Description: "The imgur link (e.g. 'https://i.imgur.com/abc123.mp4')",
				Required:    true,
			},
		},
	}
}

// convertCommandCreate is the /convert slash command definition.
func convertCommandCreate() discord.SlashCommandCreate {
	return discord.SlashCommandCreate{
		Name:        "convert",
		Description: "Convert a file or link with the goyangi encode pipeline (nothing is uploaded to the library).",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionAttachment{
				Name:        "file",
				Description: "File to convert",
			},
			discord.ApplicationCommandOptionString{
				Name:        "link",
				Description: "Link to a media file (used when no file is attached)",
			},
			discord.ApplicationCommandOptionString{
				Name:        "format",
				Description: "Output format (default: animated AVIF)",
				Choices: []discord.ApplicationCommandOptionChoiceString{
					{Name: "Animated AVIF", Value: string(hooks.ConvertAVIF)},
					{Name: "Animated WebP", Value: string(hooks.ConvertWebP)},
					{Name: "MP4 (AV1)", Value: string(hooks.ConvertMP4)},
					{Name: "MP4 (H.264 720p)", Value: string(hooks.ConvertSD)},
					{Name: "Sticker (200×200 AVIF)", Value: string(hooks.ConvertSticker)},
					{Name: "Static thumbnail", Value: string(hooks.ConvertThumb)},
				},
			},
		},
	}
}
