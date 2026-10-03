package hooks

import (
	"reflect"
	"testing"
)

func TestMetadataArgs(t *testing.T) {
	t.Run("renders sorted -metadata pairs and skips empties", func(t *testing.T) {
		got := metadataArgs(map[string]string{
			"title":            "260903 Rahee - ifeye",
			"artist":           "someuploader",
			"goyangi_filename": "", // unset field → no tag at all
			"comment":          "   ",
		})
		want := []string{
			"-metadata", "artist=someuploader",
			"-metadata", "title=260903 Rahee - ifeye",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("passes punctuation through untouched", func(t *testing.T) {
		// Each value is its own argv entry, so nothing here needs escaping —
		// and nothing must be escaped, or the tag would read back mangled.
		got := metadataArgs(map[string]string{"goyangi_filename": `my "clip", 01.webm`})
		if got[1] != `goyangi_filename=my "clip", 01.webm` {
			t.Errorf("got %q", got[1])
		}
	})

	t.Run("empty map is a no-op", func(t *testing.T) {
		if got := metadataArgs(nil); len(got) != 0 {
			t.Errorf("got %q, want none", got)
		}
	})
}
