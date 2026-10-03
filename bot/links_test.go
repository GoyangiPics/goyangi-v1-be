package bot

import (
	"reflect"
	"strings"
	"testing"
)

// TestImgurItemFromOpenGraph pins the tag-preference rules against markup
// captured from a real imgur album page.
func TestImgurItemFromOpenGraph(t *testing.T) {
	// Real page shape: twitter:image is the thumbnail ("...sh.jpg"), og:image is
	// a still frame with a query, og:video is the actual media.
	const page = `<meta name="twitter:image" data-react-helmet="true" content="https://i.imgur.com/QgD1eWsh.jpg">` +
		`<meta property="og:video" data-react-helmet="true" content="https://i.imgur.com/QgD1eWs.mp4">` +
		`<meta property="og:image" data-react-helmet="true" content="https://i.imgur.com/QgD1eWs.jpg?fbplay">`

	got, ok := imgurItemFromOpenGraph(page)
	if !ok {
		t.Fatal("expected a media item")
	}
	want := MediaItem{
		URL: "https://i.imgur.com/QgD1eWs.mp4", Filename: "QgD1eWs.mp4",
		Filetype: "gif", Mirror: "https://i.imgur.com/QgD1eWs.mp4",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("og:video should win over og:image/twitter:image\n got: %#v\nwant: %#v", got, want)
	}

	// Still-image album: no og:video, and the ?fbplay query must be dropped.
	imageOnly := `<meta property="og:image" data-react-helmet="true" content="https://i.imgur.com/QgD1eWs.jpg?fbplay">`
	got, ok = imgurItemFromOpenGraph(imageOnly)
	if !ok || got.URL != "https://i.imgur.com/QgD1eWs.jpg" || got.Filetype != "image" {
		t.Errorf("og:image fallback: got %#v (ok=%v)", got, ok)
	}

	if _, ok := imgurItemFromOpenGraph(`<meta property="og:title" content="nothing">`); ok {
		t.Error("expected no item when the page has no imgur media tags")
	}
}

func TestExtractMediaLinks(t *testing.T) {
	// Album resolution hits the network in production; stub it so these stay
	// offline and deterministic. "nope1" models an album that can't be resolved.
	orig := resolveImgurAlbum
	resolveImgurAlbum = func(pageURL string) []MediaItem {
		if strings.Contains(pageURL, "nope1") {
			return nil
		}
		item, _ := imgurItemFromOpenGraph(
			`<meta property="og:video" data-react-helmet="true" content="https://i.imgur.com/AbCdE12.mp4">`)
		return []MediaItem{item}
	}
	t.Cleanup(func() { resolveImgurAlbum = orig })

	tests := []struct {
		name    string
		content string
		want    []MediaItem
	}{
		{
			name:    "bare imgur page link becomes mp4 gif with mirror",
			content: "check https://imgur.com/AbCdE12 out",
			want: []MediaItem{{
				URL: "https://i.imgur.com/AbCdE12.mp4", Filename: "AbCdE12.mp4",
				Filetype: "gif", Mirror: "https://i.imgur.com/AbCdE12.mp4",
			}},
		},
		{
			name:    "imgur gifv normalizes to mp4",
			content: "https://i.imgur.com/AbCdE12.gifv",
			want: []MediaItem{{
				URL: "https://i.imgur.com/AbCdE12.mp4", Filename: "AbCdE12.mp4",
				Filetype: "gif", Mirror: "https://i.imgur.com/AbCdE12.mp4",
			}},
		},
		{
			name:    "imgur jpg stays an image",
			content: "https://i.imgur.com/AbCdE12.jpg",
			want: []MediaItem{{
				URL: "https://i.imgur.com/AbCdE12.jpg", Filename: "AbCdE12.jpg",
				Filetype: "image", Mirror: "https://i.imgur.com/AbCdE12.jpg",
			}},
		},
		// The slug forms below are what imgur's own share button produces for
		// anything with a title, and they were the single biggest source of lost
		// content in system_logs: the id was truncated at the first hyphen.
		{
			name:    "slug-prefixed page link keeps the hash, not the slug",
			content: "https://imgur.com/kwon-eunbi-260731-AbCdE12",
			want: []MediaItem{{
				URL: "https://i.imgur.com/AbCdE12.mp4", Filename: "AbCdE12.mp4",
				Filetype: "gif", Mirror: "https://i.imgur.com/AbCdE12.mp4",
			}},
		},
		{
			name:    "slug-prefixed direct link keeps its extension",
			content: "https://i.imgur.com/some-title-AbCdE12.jpg",
			want: []MediaItem{{
				URL: "https://i.imgur.com/AbCdE12.jpg", Filename: "AbCdE12.jpg",
				Filetype: "image", Mirror: "https://i.imgur.com/AbCdE12.jpg",
			}},
		},
		{
			name:    "imgur album resolves to the media its page points at",
			content: "https://imgur.com/a/xYz12",
			want: []MediaItem{{
				URL: "https://i.imgur.com/AbCdE12.mp4", Filename: "AbCdE12.mp4",
				Filetype: "gif", Mirror: "https://i.imgur.com/AbCdE12.mp4",
			}},
		},
		{
			// Regression: "gallery" is 7 chars, so the generic imgur id pattern
			// used to capture it and build https://i.imgur.com/gallery.mp4.
			name:    "imgur gallery link is resolved, not read as a media id",
			content: "https://imgur.com/gallery/xYz12",
			want: []MediaItem{{
				URL: "https://i.imgur.com/AbCdE12.mp4", Filename: "AbCdE12.mp4",
				Filetype: "gif", Mirror: "https://i.imgur.com/AbCdE12.mp4",
			}},
		},
		{
			name:    "unresolvable imgur album yields nothing",
			content: "https://imgur.com/a/nope1",
			want:    nil,
		},
		{
			name:    "catbox direct file",
			content: "https://files.catbox.moe/q2n9mf.webm",
			want: []MediaItem{{
				URL: "https://files.catbox.moe/q2n9mf.webm", Filename: "q2n9mf.webm",
				Filetype: "gif",
			}},
		},
		{
			name:    "pixeldrain viewer and api links normalize and dedupe",
			content: "https://pixeldrain.com/u/aBc123 and https://pixeldrain.com/api/file/aBc123",
			want: []MediaItem{{
				URL: "https://pixeldrain.com/api/file/aBc123", Filename: "aBc123",
			}},
		},
		{
			name:    "direct link with signed query keeps the query",
			content: "https://cdn.discordapp.com/attachments/1/2/clip.mp4?ex=sig&hm=abc",
			want: []MediaItem{{
				URL:      "https://cdn.discordapp.com/attachments/1/2/clip.mp4?ex=sig&hm=abc",
				Filename: "clip.mp4", Filetype: "gif",
			}},
		},
		{
			name:    "direct webp and avif links",
			content: "https://example.com/pic.webp https://example.com/pic.avif",
			want: []MediaItem{
				{URL: "https://example.com/pic.webp", Filename: "pic.webp", Filetype: "image"},
				{URL: "https://example.com/pic.avif", Filename: "pic.avif", Filetype: "image"},
			},
		},
		{
			name:    "duplicate links collapse",
			content: "https://i.imgur.com/AbCdE12.mp4 https://i.imgur.com/AbCdE12.mp4",
			want: []MediaItem{{
				URL: "https://i.imgur.com/AbCdE12.mp4", Filename: "AbCdE12.mp4",
				Filetype: "gif", Mirror: "https://i.imgur.com/AbCdE12.mp4",
			}},
		},
		{
			name:    "youtube link is not media",
			content: "source: https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			want:    nil,
		},
		{
			name:    "our own cdn links are skipped, not re-ingested",
			content: "https://cdn.goyangi.pics/v1/ive/yujin/250101-ive-yujin-ab12.mp4",
			want:    nil,
		},
		{
			name:    "own-site media links are skipped too",
			content: "https://goyangi.pics/some/pic.jpg",
			want:    nil,
		},
		{
			name: "a repost mixing our cdn with a new link keeps only the new one",
			content: "https://cdn.goyangi.pics/v1/ive/yujin/250101-ive-yujin-ab12.mp4 " +
				"https://files.catbox.moe/new123.mp4",
			want: []MediaItem{{
				URL: "https://files.catbox.moe/new123.mp4", Filename: "new123.mp4", Filetype: "gif",
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractMediaLinks(tt.content)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("extractMediaLinks(%q)\n got: %#v\nwant: %#v", tt.content, got, tt.want)
			}
		})
	}
}

func TestStripMetadataLinesKeepsMediaButDropsAttribution(t *testing.T) {
	content := "IVE fancam\n" +
		"mirror: https://i.imgur.com/MirrorX1.mp4\n" +
		"source: https://example.com/original.mp4\n" +
		"https://files.catbox.moe/real1.mp4"

	got := extractMediaLinks(stripMetadataLines(content))
	want := []MediaItem{{
		URL: "https://files.catbox.moe/real1.mp4", Filename: "real1.mp4", Filetype: "gif",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("attribution links leaked into media items:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestFiletypeClassification(t *testing.T) {
	byExt := map[string]string{
		".jpg": "image", ".jpeg": "image", ".png": "image", ".webp": "image",
		".avif": "image", ".gif": "gif", ".mp4": "gif", ".webm": "gif",
		".mov": "gif", ".txt": "", "": "",
	}
	for ext, want := range byExt {
		if got := filetypeByExt(ext); got != want {
			t.Errorf("filetypeByExt(%q) = %q, want %q", ext, got, want)
		}
	}

	byCT := map[string]string{
		"image/gif":                "gif",
		"image/png":                "image",
		"image/webp; charset=none": "image",
		"video/mp4":                "gif",
		"application/octet-stream": "",
	}
	for ct, want := range byCT {
		if got := filetypeByContentType(ct); got != want {
			t.Errorf("filetypeByContentType(%q) = %q, want %q", ct, got, want)
		}
	}
}

// TestImgurAlbumSlugIsFetchedWhole pins the album half of the same bug: the
// truncated link (imgur.com/a/kwon) is a page that exists and simply has no
// media tags, so this failed as a warning rather than an error and quietly
// accounted for most of what system_logs had recorded.
func TestImgurAlbumSlugIsFetchedWhole(t *testing.T) {
	var fetched []string
	orig := resolveImgurAlbum
	resolveImgurAlbum = func(pageURL string) []MediaItem {
		fetched = append(fetched, pageURL)
		return nil
	}
	t.Cleanup(func() { resolveImgurAlbum = orig })

	extractMediaLinks("https://imgur.com/a/kwon-eunbi-260731-AbCdE12")

	if len(fetched) != 1 {
		t.Fatalf("fetched %v, want exactly one album URL", fetched)
	}
	if want := "https://imgur.com/a/kwon-eunbi-260731-AbCdE12"; fetched[0] != want {
		t.Errorf("fetched %q, want the whole slug URL %q", fetched[0], want)
	}
}

func TestImgurIDStripsSlug(t *testing.T) {
	cases := map[string]string{
		"AbCdE12":            "AbCdE12", // no slug at all
		"kwon-eunbi-AbCdE12": "AbCdE12",
		"a-b-c-AbCdE12":      "AbCdE12",
		"trailing-":          "", // nothing addressable left
	}
	for in, want := range cases {
		if got := imgurID(in); got != want {
			t.Errorf("imgurID(%q) = %q, want %q", in, got, want)
		}
	}
}
