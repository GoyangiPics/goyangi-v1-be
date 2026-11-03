package hooks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsImgurMirror(t *testing.T) {
	yes := []string{
		"https://i.imgur.com/AbCd123.mp4",
		"https://imgur.com/AbCd123",
		" http://imgur.com/a/xyz ",
	}
	for _, m := range yes {
		if !isImgurMirror(m) {
			t.Errorf("isImgurMirror(%q) = false, want true", m)
		}
	}
	no := []string{
		"",
		"my cam",
		"https://files.catbox.moe/abc.mp4",
		"https://notimgur.com/x",
		"https://imgur.com.evil.example/x",
	}
	for _, m := range no {
		if isImgurMirror(m) {
			t.Errorf("isImgurMirror(%q) = true, want false", m)
		}
	}
}

func TestIsXStatusURL(t *testing.T) {
	yes := []string{
		"https://x.com/snowscape126/status/2116189166501548140",
		"https://twitter.com/EMITHAUS/status/2114560590769455170?s=20",
		"https://mobile.twitter.com/a/status/1/photo/2",
		" https://www.x.com/a/status/1 ",
	}
	for _, link := range yes {
		if !isXStatusURL(link) {
			t.Errorf("isXStatusURL(%q) = false, want true", link)
		}
	}
	no := []string{
		"",
		"https://x.com/snowscape126",
		"https://x.com/a/status/notanid",
		"https://fxtwitter.com/a/status/1",
		"https://x.com.evil.example/a/status/1",
		"https://imgur.com/a/status/1",
	}
	for _, link := range no {
		if isXStatusURL(link) {
			t.Errorf("isXStatusURL(%q) = true, want false", link)
		}
	}
}

// The X import's claim is honoured only with the post as `source` to back it.
func TestSiteUploadCanClaimAnXImport(t *testing.T) {
	f := newFixture(t)
	bindOriginHooks(f.app)
	const post = "https://x.com/snowscape126/status/2116189166501548140"
	create := func(name, body string, want string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/collections/contents/records", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", f.tok["u1"])
		f.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d — %s", name, rec.Code, rec.Body.String())
		}
		var got struct{ Origin string }
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if got.Origin != want {
			t.Errorf("%s: origin %q, want %q", name, got.Origin, want)
		}
	}
	base := `"title":"t","filetype":"image","uploader":"` + f.up["u1"] + `","idol":["` + f.idol + `"],"group":["` + f.groupID + `"]`
	create("claim with the post as source", `{`+base+`,"origin":"twitter","source":"`+post+`"}`, OriginTwitter)
	create("claim without a source", `{`+base+`,"origin":"twitter"}`, OriginDirect)
	create("claim with some other source", `{`+base+`,"origin":"twitter","source":"https://example.com/x"}`, OriginDirect)
	create("no claim, X source", `{`+base+`,"source":"`+post+`"}`, OriginDirect)
	create("claiming discord", `{`+base+`,"origin":"discord","source":"`+post+`"}`, OriginDirect)

	f.do(t, "an X set", http.MethodPost, "/api/collections/contents_sets/records",
		`{"title":"s","uploader":["`+f.up["u1"]+`"],"idol":["`+f.idol+`"],"group":["`+f.groupID+`"],"origin":"twitter"}`, "u1", 200, `"origin":"twitter"`)
}
