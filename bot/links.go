package bot

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"goyangi-v1-be/hooks"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// MediaItem is one downloadable media source found in a message — either an
// attachment or a recognized link. Fields left empty (Filename extension,
// Filetype) are filled in after download from the HTTP response.
type MediaItem struct {
	URL      string // direct download URL
	Filename string // filename for the PocketBase file field
	Filetype string // "image" | "gif" | "video" | "" (unknown → sniffed after download)
	Mirror   string // external mirror stored on the record (imgur links only)
}

var (
	// Hyphens are part of the path segment, not a terminator. Imgur's share URL
	// for anything with a title is slug-then-hash — imgur.com/kwon-eunbi-AbCd123
	// — and matching only [a-zA-Z0-9] stopped at the first hyphen, capturing
	// "kwon" as the media id. That built https://i.imgur.com/kwon.mp4, which
	// redirects forever ("stopped after 10 redirects") and loses the item.
	// imgurMatchToItem takes the last hyphen-separated token as the real id.
	imgurRegexp = regexp.MustCompile(`https?://(?:i\.)?imgur\.com/([a-zA-Z0-9-]{5,})(\.[a-zA-Z0-9]+)?`)
	// imgurAlbumRegexp matches album and gallery permalinks, which wrap media
	// rather than being media. These are resolved via the page's OpenGraph tags
	// and then stripped from the text, which also stops the generic imgur pass
	// above from reading the literal "gallery" path segment as a media id.
	//
	// Same hyphen problem, and worse here: the truncated link is fetched as-is,
	// so imgur.com/a/kwon-eunbi-AbCd123 became a request for imgur.com/a/kwon —
	// a page that exists, carries no og:video/og:image, and produced a "no media
	// found" warning for every slug-form album anyone posted.
	imgurAlbumRegexp = regexp.MustCompile(`https?://imgur\.com/(a|gallery)/([a-zA-Z0-9-]+)`)
	// og:video wins over og:image — on a video post og:image is a still frame.
	// twitter:image is deliberately NOT matched: it points at imgur's thumbnail,
	// whose id carries a size suffix ("QgD1eWsh.jpg" for media "QgD1eWs").
	ogVideoRegexp    = regexp.MustCompile(`<meta[^>]+property="og:video"[^>]+content="([^"]+)"`)
	ogImageRegexp    = regexp.MustCompile(`<meta[^>]+property="og:image"[^>]+content="([^"]+)"`)
	catboxRegexp     = regexp.MustCompile(`https?://files\.catbox\.moe/[a-zA-Z0-9]+\.[a-zA-Z0-9]+`)
	pixeldrainRegexp = regexp.MustCompile(`https?://pixeldrain\.com/(?:u|api/file)/([a-zA-Z0-9]+)`)
	// directRegexp matches any other direct media URL (query string allowed —
	// Discord CDN links are signed and need theirs kept).
	directRegexp = regexp.MustCompile(`https?://[^\s<>|"']+\.(?:mp4|webm|mov|webp|avif|jpe?g|png|gif)(?:\?[^\s<>|"']*)?`)
)

// resolveImgurAlbum turns an album/gallery permalink into the media items it
// holds — see imgur_album.go. Indirected through a variable so tests stay
// offline.
var resolveImgurAlbum = fetchImgurAlbumItems

// imgurAlbumClient is separate from the download client: this fetches a small
// HTML page inline during message parsing, so it gets a short leash. Same
// private-address refusal as every other outbound client (safehttp.go) — the
// regexp pins the host to imgur.com, but a redirect could point anywhere.
var imgurAlbumClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: publicOnlyTransport(),
}

// fetchImgurAlbumItem reads an album page's OpenGraph tags and returns the media
// they point at. The fallback when no IMGUR_CLIENT_ID is configured — and, as of
// 2026-09, a dead one: imgur's pages are rendered client-side and carry no
// og:video/og:image for any user agent, crawlers included. Kept so a deploy
// without the client id degrades to exactly its previous behaviour rather than
// to a new failure mode; see fetchImgurAlbumItems for the path that works.
//
// It only ever saw the album's cover item anyway — the rest of a multi-image
// album was loaded client-side — which is the other thing the API path fixes.
func fetchImgurAlbumItem(pageURL string) (MediaItem, bool) {
	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		hooks.LogBotWarning("imgur album: bad url: "+err.Error(), map[string]any{"url": pageURL})
		return MediaItem{}, false
	}
	// Imgur refuses requests without a browser User-Agent (same as downloadFile).
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36")

	resp, err := imgurAlbumClient.Do(req)
	if err != nil {
		hooks.LogBotWarning("imgur album: fetch failed: "+err.Error(), map[string]any{"url": pageURL})
		return MediaItem{}, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		hooks.LogBotWarning("imgur album: unexpected status "+resp.Status, map[string]any{"url": pageURL})
		return MediaItem{}, false
	}

	// The meta tags live in <head>; cap the read so a huge body can't stall us.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		hooks.LogBotWarning("imgur album: read failed: "+err.Error(), map[string]any{"url": pageURL})
		return MediaItem{}, false
	}

	item, ok := imgurItemFromOpenGraph(string(body))
	if !ok {
		hooks.LogBotWarning("imgur album: no og:video/og:image media found (set IMGUR_CLIENT_ID — imgur pages no longer carry them)",
			map[string]any{"url": pageURL})
	}
	return item, ok
}

// imgurItemFromOpenGraph pulls the media URL out of a page's OpenGraph tags and
// normalizes it through the same path as a plain imgur link, so filetype and
// the /revive mirror stay consistent. Split out from the fetch to stay testable.
func imgurItemFromOpenGraph(html string) (MediaItem, bool) {
	for _, re := range []*regexp.Regexp{ogVideoRegexp, ogImageRegexp} {
		m := re.FindStringSubmatch(html)
		if m == nil {
			continue
		}
		// Reuse imgurRegexp so the id/extension split (and its query-string
		// trimming, e.g. ".jpg?fbplay") behaves exactly as for a direct link.
		if im := imgurRegexp.FindStringSubmatch(m[1]); im != nil {
			if item, ok := imgurMatchToItem(im); ok {
				return item, true
			}
		}
	}
	return MediaItem{}, false
}

// hostsHandledElsewhere are skipped by the generic direct-link pass because a
// dedicated extractor above already normalizes them.
var hostsHandledElsewhere = map[string]bool{
	"imgur.com":        true,
	"i.imgur.com":      true,
	"files.catbox.moe": true,
	"pixeldrain.com":   true,
}

// filetypeByExt maps a file extension to the contents "filetype" select value
// ("" when the extension is unknown).
//
// Note: animated/video sources (mp4/webm/mov) classify as "gif", not "video".
// Discord-ingested content is short looping clips, so it goes through the gif
// pipeline — audio stripped, rendered autoplay/muted/looping with no controls.
// An explicit `filetype: video` metadata line still opts into true video (see
// the precedence in createContentRecord).
func filetypeByExt(ext string) string {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "jpg", "jpeg", "png", "webp", "avif":
		return "image"
	case "gif", "mp4", "webm", "mov":
		return "gif"
	}
	return ""
}

// filetypeByContentType maps an HTTP Content-Type to the contents "filetype"
// select value ("" when unknown). Video content types classify as "gif" for
// the same reason as filetypeByExt.
func filetypeByContentType(ct string) string {
	ct = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	switch {
	case ct == "image/gif":
		return "gif"
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.HasPrefix(ct, "video/"):
		return "gif"
	}
	return ""
}

// extFromContentType returns a filename extension for common media types.
func extFromContentType(ct string) string {
	ct = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	switch ct {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/avif":
		return ".avif"
	case "image/gif":
		return ".gif"
	case "video/mp4":
		return ".mp4"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	}
	return ""
}

// collectMedia gathers every ingestible media item from a message:
// attachments first, then recognized links in the message body (with
// metadata lines like `mirror:`/`source:` stripped so attribution links
// aren't ingested as content).
func collectMedia(m discord.Message) []MediaItem {
	var items []MediaItem
	seen := map[string]bool{}
	add := func(it MediaItem) {
		if it.URL == "" || seen[it.URL] {
			return
		}
		seen[it.URL] = true
		items = append(items, it)
	}

	for _, attach := range m.Attachments {
		add(attachmentToItem(attach))
	}
	for _, it := range extractMediaLinks(stripMetadataLines(m.Content)) {
		add(it)
	}
	return items
}

// attachmentToItem classifies a Discord attachment by its Content-Type,
// falling back to the filename extension.
func attachmentToItem(a discord.Attachment) MediaItem {
	contentType := ""
	if a.ContentType != nil {
		contentType = *a.ContentType
	}
	filetype := filetypeByContentType(contentType)
	if filetype == "" {
		filetype = filetypeByExt(path.Ext(a.Filename))
	}
	return MediaItem{
		URL:      a.URL,
		Filename: a.Filename,
		Filetype: filetype,
	}
}

// isOwnMedia reports whether a URL points at content we already host. Only
// links can be ours — Discord attachments always live on Discord's CDN.
func isOwnMedia(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return ownMediaHosts()[strings.ToLower(u.Hostname())]
}

// extractMediaLinks finds all recognized media links in the text.
func extractMediaLinks(content string) []MediaItem {
	var items []MediaItem
	seen := map[string]bool{}
	add := func(it MediaItem) {
		if it.URL == "" || seen[it.URL] {
			return
		}
		// Our own content: skip rather than re-download and re-upload it.
		if isOwnMedia(it.URL) {
			return
		}
		seen[it.URL] = true
		items = append(items, it)
	}

	// Albums/galleries first: each resolves to one media item via its page, and
	// the links are then removed so the generic imgur pass below can't capture
	// "gallery" (7 chars, so it satisfies the {5,} id rule) as a media id.
	for _, link := range imgurAlbumRegexp.FindAllString(content, -1) {
		for _, item := range resolveImgurAlbum(link) {
			add(item)
		}
	}
	content = imgurAlbumRegexp.ReplaceAllString(content, "")

	for _, match := range imgurRegexp.FindAllStringSubmatch(content, -1) {
		if item, ok := imgurMatchToItem(match); ok {
			add(item)
		}
	}

	for _, link := range catboxRegexp.FindAllString(content, -1) {
		add(MediaItem{
			URL:      link,
			Filename: path.Base(link),
			Filetype: filetypeByExt(path.Ext(link)),
		})
	}

	for _, match := range pixeldrainRegexp.FindAllStringSubmatch(content, -1) {
		id := match[1]
		// Normalize both /u/<id> and /api/file/<id> to the API download URL.
		// Extension and filetype are unknown until download.
		add(MediaItem{
			URL:      "https://pixeldrain.com/api/file/" + id,
			Filename: id,
		})
	}

	for _, link := range directRegexp.FindAllString(content, -1) {
		parsed, err := url.Parse(link)
		if err != nil || hostsHandledElsewhere[strings.ToLower(parsed.Host)] {
			continue
		}
		add(MediaItem{
			URL:      link,
			Filename: path.Base(parsed.Path),
			Filetype: filetypeByExt(path.Ext(parsed.Path)),
		})
	}

	return items
}

// imgurMatchToItem normalizes an imgur link. Imgur serves gif content as mp4,
// so .gif/.gifv/bare page links all map to the i.imgur.com .mp4 form — that
// normalized link doubles as the record's "mirror" for /revive lookups.
func imgurMatchToItem(match []string) (MediaItem, bool) {
	id, ext := imgurID(match[1]), strings.ToLower(match[2])

	// A path that is all slug and no hash isn't addressable — building a URL
	// from it produces the redirect loop this used to fall into.
	if id == "" {
		return MediaItem{}, false
	}

	if ext == "" || ext == ".gif" || ext == ".gifv" {
		link := "https://i.imgur.com/" + id + ".mp4"
		return MediaItem{URL: link, Filename: id + ".mp4", Filetype: "gif", Mirror: link}, true
	}

	link := "https://i.imgur.com/" + id + ext
	return MediaItem{
		URL:      link,
		Filename: id + ext,
		Filetype: filetypeByExt(ext),
		Mirror:   link,
	}, true
}

// imgurID pulls the media hash out of a path segment that may be slug-prefixed.
//
// Imgur renders a titled post as "<slug>-<hash>" and the hash is always the
// final token; ids themselves never contain a hyphen, so the last token is the
// id whether or not a slug is present.
func imgurID(segment string) string {
	if i := strings.LastIndex(segment, "-"); i >= 0 {
		return segment[i+1:]
	}
	return segment
}

// ─── Discord message links ───────────────────────────────────────────────────

// discordMessageLinkRegexp matches a Discord message jump link.
//
// The host varies by client build (ptb./canary.) and discordapp.com is still
// served for old links. "@me" in the guild slot means a DM, which is matched here
// only so it can be rejected with a clear message rather than "not a link".
var discordMessageLinkRegexp = regexp.MustCompile(
	`https?://(?:ptb\.|canary\.)?discord(?:app)?\.com/channels/(\d+|@me)/(\d+)/(\d+)`)

// messageRef identifies a single Discord message.
type messageRef struct {
	guildID   snowflake.ID
	channelID snowflake.ID
	messageID snowflake.ID
}

// parseMessageLink extracts a message reference from a jump link.
//
// Tolerant of surrounding text and whitespace, because the value arrives from a
// command option that people paste into.
func parseMessageLink(raw string) (messageRef, error) {
	m := discordMessageLinkRegexp.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return messageRef{}, fmt.Errorf(
			"that doesn't look like a message link — right-click the message → Copy Message Link")
	}
	if m[1] == "@me" {
		// Ingestion stamps a guild message URL onto every record it creates (see
		// fillMessageDefaults), so a DM has nothing usable to record.
		return messageRef{}, fmt.Errorf("DM links can't be ingested — the message has to be in a server")
	}

	guildID, err := snowflake.Parse(m[1])
	if err != nil {
		return messageRef{}, fmt.Errorf("unreadable server id in that link")
	}
	channelID, err := snowflake.Parse(m[2])
	if err != nil {
		return messageRef{}, fmt.Errorf("unreadable channel id in that link")
	}
	messageID, err := snowflake.Parse(m[3])
	if err != nil {
		return messageRef{}, fmt.Errorf("unreadable message id in that link")
	}

	return messageRef{guildID: guildID, channelID: channelID, messageID: messageID}, nil
}
