package bot

import "testing"

// The post API lists every item of an album, and each one is normalised the way
// a directly-posted imgur link is: videos and gifs to the i.imgur.com mp4
// mirror form, stills kept as they are.
func TestImgurItemsFromAPI(t *testing.T) {
	body := []byte(`{
		"id": "myRGRUW", "title": "kirin jiwoo 1", "image_count": 3,
		"media": [
			{"id": "kQP8CAw", "type": "video", "mime_type": "video/mp4", "ext": "mp4",
			 "url": "https://i.imgur.com/kQP8CAw.mp4", "width": 640, "height": 854},
			{"id": "AbCdE12", "type": "image", "mime_type": "image/gif", "ext": "gif",
			 "url": "https://i.imgur.com/AbCdE12.gif"},
			{"id": "ZyXwV98", "type": "image", "mime_type": "image/jpeg", "ext": "jpg",
			 "url": "https://i.imgur.com/ZyXwV98.jpg"}
		]
	}`)
	got, err := imgurItemsFromAPI(body)
	if err != nil {
		t.Fatal(err)
	}
	want := []MediaItem{
		{URL: "https://i.imgur.com/kQP8CAw.mp4", Filename: "kQP8CAw.mp4", Filetype: "gif", Mirror: "https://i.imgur.com/kQP8CAw.mp4"},
		{URL: "https://i.imgur.com/AbCdE12.mp4", Filename: "AbCdE12.mp4", Filetype: "gif", Mirror: "https://i.imgur.com/AbCdE12.mp4"},
		{URL: "https://i.imgur.com/ZyXwV98.jpg", Filename: "ZyXwV98.jpg", Filetype: "image", Mirror: "https://i.imgur.com/ZyXwV98.jpg"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d items, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestImgurItemsFromAPIEmptyAndBroken(t *testing.T) {
	if got, err := imgurItemsFromAPI([]byte(`{"media": []}`)); err != nil || len(got) != 0 {
		t.Errorf("empty album: got %v, %v", got, err)
	}
	if _, err := imgurItemsFromAPI([]byte(`not json`)); err == nil {
		t.Error("broken body: want an error")
	}
}

// The album regexp now captures the kind, which decides the API endpoint; the
// id is still the last hyphen-separated token of a slugged path.
func TestImgurAlbumRegexpKinds(t *testing.T) {
	for _, tc := range []struct{ url, kind, id string }{
		{"https://imgur.com/a/myRGRUW", "a", "myRGRUW"},
		{"https://imgur.com/gallery/kwon-eunbi-AbCd123", "gallery", "AbCd123"},
	} {
		m := imgurAlbumRegexp.FindStringSubmatch(tc.url)
		if m == nil || m[1] != tc.kind || imgurID(m[2]) != tc.id {
			t.Errorf("%s: got %v, want kind %s id %s", tc.url, m, tc.kind, tc.id)
		}
	}
}
