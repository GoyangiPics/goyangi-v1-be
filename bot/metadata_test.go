package bot

import (
	"strings"
	"testing"
)

func TestExtractMetadata(t *testing.T) {
	t.Run("parses key-value lines", func(t *testing.T) {
		content := strings.Join([]string{
			"idol: Wonyoung",
			"group: IVE",
			"tags: fancam, 4k",
			"title: Love Dive stage",
			"date: 240115",
			"uploader: someone_else",
		}, "\n")

		var m Metadata
		if err := extractMetadata(content, &m); err != nil {
			t.Fatal(err)
		}
		if m.Idol != "Wonyoung" || m.Group != "IVE" || m.Tags != "fancam, 4k" ||
			m.Title != "Love Dive stage" || m.Date != "240115" || m.Uploader != "someone_else" {
			t.Errorf("unexpected metadata: %+v", m)
		}
	})

	// "source: my cam" is a note, not a link. Left in place it failed the record's
	// url field and sank every item in the message.
	t.Run("drops source and mirror lines that are not URLs", func(t *testing.T) {
		content := "idol: Ahyeon\ngroup: BABYMONSTER\nsource: my cam\nmirror: imgur"
		var m Metadata
		if err := extractMetadata(content, &m); err != nil {
			t.Fatal(err)
		}
		if m.Source != "" || m.Mirror != "" {
			t.Errorf("source = %q, mirror = %q; want both empty", m.Source, m.Mirror)
		}
	})

	t.Run("keeps a source line that is a URL", func(t *testing.T) {
		content := "idol: Ahyeon\ngroup: BABYMONSTER\nsource: https://x.com/kuro/status/1"
		var m Metadata
		if err := extractMetadata(content, &m); err != nil {
			t.Fatal(err)
		}
		if m.Source != "https://x.com/kuro/status/1" {
			t.Errorf("source = %q", m.Source)
		}
	})

	// The regexp accepts the schemeless form people paste; the url field does not.
	t.Run("gives a schemeless youtube fallback a scheme", func(t *testing.T) {
		content := "idol: Yujin\ngroup: IVE\nyoutu.be/dQw4w9WgXcQ"
		var m Metadata
		if err := extractMetadata(content, &m); err != nil {
			t.Fatal(err)
		}
		if m.Source != "https://youtu.be/dQw4w9WgXcQ" {
			t.Errorf("source = %q", m.Source)
		}
	})

	t.Run("title and source fallbacks", func(t *testing.T) {
		content := "idol: Yujin\ngroup: IVE\nhttps://youtu.be/dQw4w9WgXcQ"
		var m Metadata
		if err := extractMetadata(content, &m); err != nil {
			t.Fatal(err)
		}
		// "Idols - Groups", not "... from ...": this string becomes the record's
		// title and therefore the heading on its page and in its social embed,
		// where a dash reads as a label rather than a sentence fragment.
		if m.Title != "Yujin - IVE" {
			t.Errorf("title fallback = %q", m.Title)
		}
		if !strings.Contains(m.Source, "dQw4w9WgXcQ") {
			t.Errorf("source fallback = %q", m.Source)
		}
	})

	t.Run("missing idol and group errors", func(t *testing.T) {
		var m Metadata
		err := extractMetadata("just some text", &m)
		if err == nil || !strings.Contains(err.Error(), "idol, group") {
			t.Errorf("want missing-metadata error, got %v", err)
		}
	})

	t.Run("value keeps colons after the first", func(t *testing.T) {
		var m Metadata
		_ = extractMetadata("idol: A\ngroup: B\ntitle: stage: part 2", &m)
		if m.Title != "stage: part 2" {
			t.Errorf("title = %q", m.Title)
		}
	})
}

func TestRolesToMetadata(t *testing.T) {
	t.Run("multi-word idol names survive", func(t *testing.T) {
		m := rolesToMetadata([]string{"Chae Won [LE SSERAFIM]", "Yujin [IVE]"})
		if m.Idol != "Chae Won, Yujin" {
			t.Errorf("idols = %q", m.Idol)
		}
		if m.Group != "LE SSERAFIM, IVE" {
			t.Errorf("groups = %q", m.Group)
		}
	})

	t.Run("non-matching roles are skipped and names deduped", func(t *testing.T) {
		m := rolesToMetadata([]string{"Moderator", "Yujin [IVE]", "Gaeul [IVE]"})
		if m.Idol != "Yujin, Gaeul" || m.Group != "IVE" {
			t.Errorf("got idol=%q group=%q", m.Idol, m.Group)
		}
	})
}

func TestSplitTrim(t *testing.T) {
	got := splitTrim(" a, b ,, c ")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("splitTrim = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitTrim[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestAutoTitle pins the shape runIngestion compares against to tell a generated
// title from one the poster wrote. If these drift, a retitle either never fires
// or overwrites somebody's own words.
func TestAutoTitle(t *testing.T) {
	if got := autoTitle("Kwon Eunbi", "Solo"); got != "Kwon Eunbi - Solo" {
		t.Errorf("autoTitle = %q", got)
	}
	if got := autoTitle("", ""); got != "" {
		t.Errorf("autoTitle of nothing = %q, want empty so it can't match a real title", got)
	}

	// The generated title has to equal what extractMetadata produced, or the
	// comparison in runIngestion silently stops matching.
	var m Metadata
	if err := extractMetadata("idol: Eunbi\ngroup: IZONE", &m); err != nil {
		t.Fatal(err)
	}
	if m.Title != autoTitle(m.Idol, m.Group) {
		t.Errorf("extractMetadata title %q != autoTitle %q", m.Title, autoTitle(m.Idol, m.Group))
	}

	// An explicit title must NOT look generated, or it would be overwritten.
	var explicit Metadata
	if err := extractMetadata("idol: Eunbi\ngroup: IZONE\ntitle: airport pics", &explicit); err != nil {
		t.Fatal(err)
	}
	if explicit.Title == autoTitle(explicit.Idol, explicit.Group) {
		t.Error("an explicit title compares equal to the generated one, so it would be clobbered")
	}
}
