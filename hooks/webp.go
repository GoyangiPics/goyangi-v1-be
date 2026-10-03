package hooks

// WebP preview generation — an alternative to the AVIF path for callers that
// need broader device compatibility. WebP (VP8-based) is a generation behind
// AVIF (AV1-based): at matched visual quality its files run ~1.5–3× larger and
// it can't hit AVIF's size:speed point. The trade-off is reach — animated WebP
// decodes on effectively every browser and OS in the wild, including the older
// devices where AVIF support is still spotty.
//
// Unlike the avifenc path (avifenc.go), WebP needs no extra binary: ffmpeg's
// libwebp encoder muxes animated WebP directly. The only requirement is an
// ffmpeg built with --enable-libwebp (verify with `ffmpeg -hide_banner
// -encoders | grep libwebp` — the `libwebp` encoder must be listed, not just
// the `webp` muxer).

// webpScaleFilter boxes the preview at width ≤1152, height ≤768, never
// upscaling. Landscape binds on the width and comes out 1152×648. Portrait
// binds on the height: 9:16 lands at 432×768, a 934×1920 fancam at 374×768.
//
// The height cap used to be 692, which put that same fancam at 336×692 — the
// preview-path version of the bug stillScaleCap's comment describes: an
// asymmetric box acts as a landscape cap silently applied to portrait content,
// and portrait is most of what gets uploaded. Portrait carries ~1.23× the
// pixels it did; landscape is unchanged. (864 was tried first — it matches
// what imgur serves Discord for the same clip, 416×854 — and was walked back
// to 768 to keep the decode budget closer to the original; it remains the
// obvious next step up.)
//
// Animated WebP is decoded entirely in software (there is no hardware path as
// there is for video), so decode cost is pixels × frames and older devices
// feel every pixel. This is why the WebP box is deliberately smaller than the
// AVIF preview's (avifScaleFilter, long edge ≤1280): AVIF decodes through the
// platform's hardware-assisted AV1 path and does not share this budget. If
// reports of lag on old devices come back, this height is the knob.
//
// The min(iw,…)/min(ih,…) terms stop the filter from upscaling small sources
// (a fixed WxH box with force_original_aspect_ratio=decrease scales UP
// anything smaller than the box). It is the same construction as
// scaleDown1080p and stillScaleCap.
//
// The FE is unaffected by any of this: the output aspect ratio is the source's,
// and the cards size from CSS (`w-full`). Only the intrinsic pixel size differs.
//
// force_divisible_by=2 is required because yuv420p needs even dimensions and
// portrait sources will otherwise land on an odd width.
//
// The trailing format=yuv420p asserts no alpha plane reaches the encoder. Note
// that the shipped file still reports Alpha:1 in its VP8X header — verified
// locally with webpinfo: the flag is set unconditionally by ffmpeg's muxer, but
// the file contains zero ALPH chunks, so no alpha data is encoded and the flag
// is harmless.
const webpScaleFilter = "scale='min(iw,1152)':'min(ih,768)':force_original_aspect_ratio=decrease:force_divisible_by=2,setsar=1,format=yuv420p"

// generateAnimatedWebP creates an animated WebP preview from video bytes — the
// WebP counterpart to generateAnimatedAVIF. Same input contract (typically the
// already-transcoded AV1 MP4 bytes) and same output role (the `preview` file).
//
// Settings (size-priority, mirroring the AVIF path's philosophy):
//   - libwebp (ffmpeg's built-in animated-WebP encoder — no extra binary)
//   - quality 75 (0–100; libwebp's default). Was 72. Bake-off on six fancams
//     put q78 at +15% bytes for +0.8 VMAF over q72 with no encode-time cost,
//     and q75 measured at +3.2% bytes over q72. Quality does not change
//     decode cost (that is pixels × frames), so this is the one knob that can
//     be raised without touching the old-device budget.
//   - compression_level 5 — NOTE: currently a no-op. ffmpeg's libwebp wrapper
//     (libwebpenc_common.c, ff_libwebp_encode_init_common) builds the config
//     from the preset when one is given and then overwrites compression_level
//     with the preset's method, which is libwebp's default of 4. Confirmed
//     with -loglevel debug: "quality=72.0 method=4". Quality is NOT affected
//     (it is passed into WebPConfigPreset). So the effective encode is
//     q72 / method 4 / picture preset. Bake-off (6 fancams, SSIM+VMAF vs the
//     AV1 source): dropping the preset to make method 5 or 6 take effect
//     changes size by ≤2.5% and VMAF by ≤+0.4 — inside noise — at 1.1–1.6×
//     encode time, so it is not worth it. The flag stays so that removing the
//     preset later gives the documented method; it is not a knob today.
//   - preset picture (biases entropy coder + filter sharpness for photographic
//     content). Preserves the explicit quality; overrides method — see above.
//   - yuv420p (WebP is 8-bit only — there is no 10-bit path like AVIF's), and
//     asserted a second time in webpScaleFilter to keep alpha out. The output's
//     VP8X header reports ALPHA=1 regardless; that is a muxer flag, not an
//     alpha plane — see the note on webpScaleFilter.
//   - loop 0 (animate forever; without it ffmpeg writes a play-once file)
//   - fpsmax 30, audio stripped — identical to the AVIF path
//
// Note: no still-picture flag here (the WebP equivalent of avif=1). Feeding
// libwebp multiple frames with -loop makes it emit an animated WebP; a single
// frame would collapse to a static image — that case is generateStaticWebP.
func generateAnimatedWebP(content []byte, srcExt string) ([]byte, error) {
	return runFFmpeg(content, srcExt, ".webp",
		"-fpsmax", "30",
		"-vf", webpScaleFilter,
		"-c:v", "libwebp",
		"-pix_fmt", "yuv420p",
		"-quality", "75",
		"-compression_level", "5",
		"-preset", "picture",
		"-loop", "0",
		"-an",
	)
}

// generateStaticWebP extracts the first frame as a small static WebP poster —
// the WebP counterpart to generateStaticPreview. Used for the `static` field
// when a gif's preview_format is "webp", so a WebP record is fully AVIF-free
// (preview.webp + static.webp) and renders end-to-end on AVIF-incapable
// devices — otherwise the poster would be an AVIF those same devices can't
// decode, defeating the point of choosing WebP.
//
// Single frame. compression_level 6 is a no-op here for the same reason as in
// generateAnimatedWebP (-preset overrides it to method 4); quality is nudged up
// vs. the animated preview since a crisp poster is worth the few extra KB.
func generateStaticWebP(content []byte, srcExt string) ([]byte, error) {
	return runFFmpeg(content, srcExt, ".webp",
		"-vframes", "1",
		"-vf", webpScaleFilter,
		"-c:v", "libwebp",
		"-pix_fmt", "yuv420p",
		"-quality", "78",
		"-compression_level", "6",
		"-preset", "picture",
		"-an",
	)
}
