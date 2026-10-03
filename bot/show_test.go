package bot

import "testing"

func TestResolveLinkSDFormat(t *testing.T) {
	get := func(fields map[string]string) func(string) string {
		return func(field string) string { return fields[field] }
	}

	t.Run("prefers the sd rendition", func(t *testing.T) {
		link := resolveLink(formatSD, get(map[string]string{
			"sd":       "https://cdn/clip-sd.mp4",
			"original": "https://cdn/clip.mp4",
			"preview":  "https://cdn/clip.avif",
		}))
		if link != "https://cdn/clip-sd.mp4" {
			t.Fatalf("got %q", link)
		}
	})

	t.Run("falls back to the AV1 original when sd is absent", func(t *testing.T) {
		// sd is best-effort in the pipeline and missing entirely on records that
		// predate the rendition; handing back nothing would embed nothing.
		link := resolveLink(formatSD, get(map[string]string{
			"original": "https://cdn/clip.mp4",
			"preview":  "https://cdn/clip.avif",
		}))
		if link != "https://cdn/clip.mp4" {
			t.Fatalf("got %q", link)
		}
	})

	t.Run("falls through to preview then mirror", func(t *testing.T) {
		if link := resolveLink(formatSD, get(map[string]string{
			"preview": "https://cdn/clip.avif",
			"mirror":  "https://i.imgur.com/clip.mp4",
		})); link != "https://cdn/clip.avif" {
			t.Fatalf("preview: got %q", link)
		}
		if link := resolveLink(formatSD, get(map[string]string{
			"mirror": "https://i.imgur.com/clip.mp4",
		})); link != "https://i.imgur.com/clip.mp4" {
			t.Fatalf("mirror: got %q", link)
		}
	})

	t.Run("returns empty when the record has nothing", func(t *testing.T) {
		if link := resolveLink(formatSD, get(map[string]string{})); link != "" {
			t.Fatalf("got %q", link)
		}
	})
}

func TestShowButtonRow(t *testing.T) {
	t.Run("builds one button per format for a normal id", func(t *testing.T) {
		row, ok := showButtonRow("260727-ive-leeseo-a1b2c3d4")
		if !ok {
			t.Fatal("expected buttons for a normal-length id")
		}
		if len(row.Components) != len(showFormats) {
			t.Fatalf("got %d buttons, want %d", len(row.Components), len(showFormats))
		}
	})

	t.Run("declines rather than exceeding Discord's custom id limit", func(t *testing.T) {
		// generateContentId derives ids from group/idol names, so a many-idol
		// "mix" record can in principle run long. Sending an over-length custom
		// id makes Discord reject the whole message.
		long := "mix-260727"
		for len(long) < discordCustomIDLimit {
			long += "-someidolname"
		}
		if _, ok := showButtonRow(long); ok {
			t.Fatal("expected showButtonRow to decline an over-long id")
		}
	})
}

func TestShowFormatByKey(t *testing.T) {
	for _, f := range showFormats {
		format, label, ok := showFormatByKey(f.key)
		if !ok || format != f.format || label != f.label {
			t.Errorf("showFormatByKey(%q) = (%q, %q, %v)", f.key, format, label, ok)
		}
	}
	if _, _, ok := showFormatByKey("nope"); ok {
		t.Error("unknown key should not resolve")
	}
}
