// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package connector

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	log "github.com/rs/zerolog/log"
	"go.mau.fi/util/ffmpeg"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"github.com/lrhodin/imessage/pkg/rustpushgo"
)

// ---------------------------------------------------------------------------
// Command definitions
// ---------------------------------------------------------------------------

var cmdSharedAlbums = &commands.FullHandler{
	Name: "shared-albums",
	Func: fnSharedAlbums,
	Help: commands.HelpMeta{
		Section:     HelpSectionSharedStreams,
		Description: "Browse shared albums — pick an album, then pick assets to download.",
	},
	RequiresLogin: true,
}

var cmdSharedSubscribe = &commands.FullHandler{
	Name: "shared-subscribe",
	Func: fnSharedSubscribe,
	Help: commands.HelpMeta{
		Section:     HelpSectionSharedStreams,
		Description: "Subscribe to a shared album by its album ID so the bridge watches it for new assets.",
		Args:        "<album-id>",
	},
	RequiresLogin: true,
}

var cmdSharedSubscribeToken = &commands.FullHandler{
	Name: "shared-subscribe-token",
	Func: fnSharedSubscribeToken,
	Help: commands.HelpMeta{
		Section:     HelpSectionSharedStreams,
		Description: "Subscribe to a shared album using the one-time invitation token from an iCloud share URL.",
		Args:        "<token>",
	},
	RequiresLogin: true,
}

var cmdSharedUnsubscribe = &commands.FullHandler{
	Name: "shared-unsubscribe",
	Func: fnSharedUnsubscribe,
	Help: commands.HelpMeta{
		Section:     HelpSectionSharedStreams,
		Description: "Unsubscribe from a shared album by album ID so the bridge stops watching it.",
		Args:        "<album-id>",
	},
	RequiresLogin: true,
}

var cmdSharedState = &commands.FullHandler{
	Name: "shared-state",
	Func: fnSharedState,
	Help: commands.HelpMeta{
		Section:     HelpSectionSharedStreams,
		Description: "Dump raw Shared Streams state (subscriptions, asset metadata) as JSON — debugging only.",
	},
	RequiresLogin: true,
}

var cmdSharedAssetsJSON = &commands.FullHandler{
	Name: "shared-assets-json",
	Func: fnSharedAssetsJSON,
	Help: commands.HelpMeta{
		Section:     HelpSectionSharedStreams,
		Description: "Export full asset metadata as JSON for specific assets in an album — debugging only.",
		Args:        "<album-id> <asset-guid...>",
	},
	RequiresLogin: true,
}

var cmdSharedDeleteAssets = &commands.FullHandler{
	Name: "shared-delete-assets",
	Func: fnSharedDeleteAssets,
	Help: commands.HelpMeta{
		Section:     HelpSectionSharedStreams,
		Description: "Delete specific assets from a shared album by asset GUID.",
		Args:        "<album-id> <asset-guid...>",
	},
	RequiresLogin: true,
}

var cmdDeleteRoom = &commands.FullHandler{
	Name: "delete-room",
	Func: fnDeleteRoom,
	Help: commands.HelpMeta{
		Section:     HelpSectionSharedStreams,
		Description: "Delete this shared album download room. Run from inside the album room.",
	},
	RequiresLogin: true,
}

func fnDeleteRoom(ce *commands.Event) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No active login found.")
		return
	}
	client, ok := login.Client.(*IMClient)
	if !ok || client == nil {
		ce.Reply("Bridge client not available.")
		return
	}

	client.sharedAlbumRoomsMu.Lock()
	var foundAlbum string
	for albumGUID, roomID := range client.sharedAlbumRooms {
		if roomID == ce.RoomID {
			foundAlbum = albumGUID
			break
		}
	}
	if foundAlbum == "" {
		client.sharedAlbumRoomsMu.Unlock()
		ce.Reply("This is not a shared album download room.")
		return
	}
	delete(client.sharedAlbumRooms, foundAlbum)
	client.sharedAlbumRoomsMu.Unlock()

	err := client.Main.Bridge.Bot.DeleteRoom(ce.Ctx, ce.RoomID, false)
	if err != nil {
		ce.Reply("Failed to delete room: %v", err)
		return
	}
}

// ---------------------------------------------------------------------------
// Step 1: !shared-albums — numbered album picker
// ---------------------------------------------------------------------------

func fnSharedAlbums(ce *commands.Event) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No active login found.")
		return
	}
	client, ok := login.Client.(*IMClient)
	if !ok || client == nil || client.client == nil {
		ce.Reply("Bridge client not available.")
		return
	}

	var albums []rustpushgo.SharedAlbumInfo
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("shared streams client panicked: %v", r)
			}
		}()
		ss, initErr := client.client.GetSharedstreamsClient()
		if initErr != nil {
			err = initErr
			return
		}
		albums = ss.ListAlbums()
	}()

	if err != nil {
		ce.Reply("Failed to get shared albums: %v", err)
		return
	}

	if len(albums) == 0 {
		ce.Reply("You are not subscribed to any shared albums.")
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**Shared Albums (%d)**\n\n", len(albums)))
	for i, a := range albums {
		label := a.Albumguid
		if a.Name != nil && *a.Name != "" {
			label = *a.Name
		}
		extra := ""
		if a.Email != nil && *a.Email != "" {
			extra = fmt.Sprintf(" (shared by %s)", *a.Email)
		}
		sb.WriteString(fmt.Sprintf("%d. **%s**%s\n", i+1, label, extra))
	}
	sb.WriteString("\nReply with a number to browse, or `$cmdprefix cancel` to cancel.")
	ce.Reply(sb.String())

	commands.StoreCommandState(ce.User, &commands.CommandState{
		Action: "select shared album",
		Next: commands.MinimalCommandHandlerFunc(func(ce *commands.Event) {
			handleAlbumSelection(ce, client, albums)
		}),
		Cancel: func() {},
	})
}

// ---------------------------------------------------------------------------
// Step 2: asset list after album selection
// ---------------------------------------------------------------------------

func handleAlbumSelection(ce *commands.Event, client *IMClient, albums []rustpushgo.SharedAlbumInfo) {
	n, err := strconv.Atoi(strings.TrimSpace(ce.RawArgs))
	if err != nil || n < 1 || n > len(albums) {
		ce.Reply("Please reply with a number between 1 and %d, or `$cmdprefix cancel` to cancel.", len(albums))
		return
	}

	commands.StoreCommandState(ce.User, nil)

	chosen := albums[n-1]
	albumGUID := chosen.Albumguid
	albumName := chosen.Albumguid
	if chosen.Name != nil && *chosen.Name != "" {
		albumName = *chosen.Name
	}

	ss, ssOk := sharedStreamsClientFromEvent(ce)
	if !ssOk {
		return
	}

	// Single authenticated call — reads cached GUIDs from state (no network)
	// then fetches full asset metadata. Avoids the double-auth-request that
	// caused TokenMissing.
	var assets []rustpushgo.SharedAssetInfo
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("shared streams client panicked: %v", r)
			}
		}()
		assets, err = ss.GetAlbumAssets(albumGUID)
	}()
	if err != nil {
		ce.Reply("Failed to fetch assets for **%s**: %v", albumName, err)
		return
	}
	if len(assets) == 0 {
		ce.Reply("**%s** is empty.", albumName)
		return
	}

	// Precompute parsed DateCreated once per asset \u2014 used for sorting and display.
	type assetWithTime struct {
		asset rustpushgo.SharedAssetInfo
		t     time.Time
	}
	withTimes := make([]assetWithTime, len(assets))
	for i, a := range assets {
		withTimes[i].asset = a
		withTimes[i].t, _ = time.Parse(time.RFC3339, a.DateCreated)
	}

	sort.SliceStable(withTimes, func(i, j int) bool { return withTimes[i].t.Before(withTimes[j].t) })

	shown := withTimes
	truncated := false
	if len(shown) > 50 {
		shown = shown[len(shown)-50:]
		truncated = true
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("**%s** \u2014 %d asset(s)", albumName, len(assets)))
	if truncated {
		sb.WriteString(fmt.Sprintf(" (showing latest 50 of %d)", len(assets)))
	}
	sb.WriteString("\n\n")

	for i, awt := range shown {
		a := awt.asset
		dims := ""
		if a.Width != "" && a.Height != "" && a.Width != "0" && a.Height != "0" {
			dims = fmt.Sprintf(", %s\u00d7%s", a.Width, a.Height)
		}
		date := ""
		if !awt.t.IsZero() {
			date = fmt.Sprintf(", %s", awt.t.Format("2006-01-02"))
		}
		mediaLabel := a.MediaType
		if mediaLabel == "" {
			mediaLabel = "file"
		}
		sb.WriteString(fmt.Sprintf("%d. **%s** (%s%s%s)\n", i+1, a.Filename, mediaLabel, dims, date))
	}
	sb.WriteString("\nReply with number(s), a range (1-5), or `all` to download. `$cmdprefix cancel` to cancel.")
	ce.Reply(sb.String())

	shownAssets := make([]rustpushgo.SharedAssetInfo, len(shown))
	for i, awt := range shown {
		shownAssets[i] = awt.asset
	}

	commands.StoreCommandState(ce.User, &commands.CommandState{
		Action: "select shared album assets",
		Next: commands.MinimalCommandHandlerFunc(func(ce *commands.Event) {
			handleAssetSelection(ce, client, ss, albumGUID, albumName, shownAssets)
		}),
		Cancel: func() {},
	})
}

// ---------------------------------------------------------------------------
// Step 3: download selected assets
// ---------------------------------------------------------------------------

func handleAssetSelection(ce *commands.Event, client *IMClient, ss *rustpushgo.WrappedSharedStreamsClient, albumGUID, albumName string, assets []rustpushgo.SharedAssetInfo) {
	commands.StoreCommandState(ce.User, nil)

	input := strings.TrimSpace(ce.RawArgs)
	indices, err := parseAssetSelection(input, len(assets))
	if err != nil {
		ce.Reply("%v", err)
		return
	}

	selectedGUIDs := make([]string, 0, len(indices))
	selectedNames := make([]string, 0, len(indices))
	for _, idx := range indices {
		selectedGUIDs = append(selectedGUIDs, assets[idx].Assetguid)
		name := assets[idx].Filename
		if name == "" {
			name = assets[idx].Assetguid
		}
		selectedNames = append(selectedNames, name)
	}

	roomID, err := client.getOrCreateAlbumRoom(ce.Ctx, albumGUID, albumName)
	if err != nil {
		ce.Reply("Failed to create album room: %v", err)
		return
	}

	ce.Reply("Downloading %d asset(s) from **%s** \u2014 check the dedicated room for progress.", len(selectedGUIDs), albumName)

	// Use background context — ce.Ctx is cancelled when the command handler returns.
	go client.downloadSharedAlbumAssets(context.Background(), ss, albumGUID, albumName, selectedGUIDs, selectedNames, roomID)
}

// parseAssetSelection parses user input like "1", "1,3,5", "1-5", or "all"
// into zero-based indices.
func parseAssetSelection(input string, total int) ([]int, error) {
	input = strings.ToLower(strings.TrimSpace(input))
	if input == "all" {
		indices := make([]int, total)
		for i := range indices {
			indices[i] = i
		}
		return indices, nil
	}

	seen := make(map[int]bool)
	var indices []int

	for _, part := range strings.Split(input, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if dashIdx := strings.Index(part, "-"); dashIdx > 0 {
			startStr := strings.TrimSpace(part[:dashIdx])
			endStr := strings.TrimSpace(part[dashIdx+1:])
			start, err1 := strconv.Atoi(startStr)
			end, err2 := strconv.Atoi(endStr)
			if err1 != nil || err2 != nil || start < 1 || end < 1 || start > total || end > total {
				return nil, fmt.Errorf("Invalid range `%s`. Use numbers between 1 and %d.", part, total)
			}
			if start > end {
				start, end = end, start
			}
			for i := start; i <= end; i++ {
				if !seen[i-1] {
					seen[i-1] = true
					indices = append(indices, i-1)
				}
			}
		} else {
			n, err := strconv.Atoi(part)
			if err != nil || n < 1 || n > total {
				return nil, fmt.Errorf("Invalid selection `%s`. Use numbers between 1 and %d, ranges (1-5), or `all`.", part, total)
			}
			if !seen[n-1] {
				seen[n-1] = true
				indices = append(indices, n-1)
			}
		}
	}

	if len(indices) == 0 {
		return nil, fmt.Errorf("No assets selected. Reply with number(s), a range (1-5), or `all`.")
	}
	return indices, nil
}

// ---------------------------------------------------------------------------
// Dedicated album room
// ---------------------------------------------------------------------------

func (c *IMClient) getOrCreateAlbumRoom(ctx context.Context, albumGUID, albumName string) (id.RoomID, error) {
	c.sharedAlbumRoomsMu.Lock()
	defer c.sharedAlbumRoomsMu.Unlock()

	if roomID, ok := c.sharedAlbumRooms[albumGUID]; ok {
		return roomID, nil
	}

	displayName := albumName
	if displayName == "" || displayName == albumGUID {
		displayName = "Shared Album"
	}

	roomID, err := c.Main.Bridge.Bot.CreateRoom(ctx, &mautrix.ReqCreateRoom{
		Name:                  displayName,
		Topic:                 fmt.Sprintf("Shared album download \u2014 %s", displayName),
		IsDirect:              true,
		Preset:                "trusted_private_chat",
		Invite:                []id.UserID{c.UserLogin.User.MXID},
		BeeperAutoJoinInvites: true,
	})
	if err != nil {
		return "", fmt.Errorf("create room: %w", err)
	}

	c.sharedAlbumRooms[albumGUID] = roomID
	return roomID, nil
}

// ---------------------------------------------------------------------------
// Background download loop
// ---------------------------------------------------------------------------

func (c *IMClient) downloadSharedAlbumAssets(ctx context.Context, ss *rustpushgo.WrappedSharedStreamsClient, albumGUID, albumName string, assetGUIDs, assetNames []string, roomID id.RoomID) {
	intent := c.Main.Bridge.Bot
	logger := c.Main.Bridge.Log.With().
		Str("component", "shared_album_download").
		Str("album", albumGUID).
		Logger()

	// Send a welcome/help notice into the album room.
	welcome := format.RenderMarkdown(fmt.Sprintf(
		"Downloading %d asset(s) from **%s**.\n\nWhen you're done, send `!im delete-room` here to delete this room.",
		len(assetGUIDs), albumName), true, false)
	welcome.MsgType = event.MsgNotice
	_, _ = intent.SendMessage(ctx, roomID, event.EventMessage, &event.Content{Parsed: welcome}, nil)

	var failed int

	for i, guid := range assetGUIDs {
		name := assetNames[i]
		logger.Info().
			Int("index", i+1).
			Int("total", len(assetGUIDs)).
			Str("filename", name).
			Msg("Downloading shared album asset")

		data, err := safeSharedAlbumDownload(ss, albumGUID, guid)
		if err != nil {
			logger.Warn().Err(err).Str("asset", guid).Msg("Failed to download shared album asset")
			failed++
			continue
		}
		if len(data) == 0 {
			logger.Warn().Str("asset", guid).Msg("Shared album asset returned empty data")
			failed++
			continue
		}

		content := c.processSharedAlbumAsset(ctx, logger, intent, data, name)
		if content == nil {
			failed++
			continue
		}

		_, sendErr := intent.SendMessage(ctx, roomID, event.EventMessage, &event.Content{
			Parsed: content,
		}, nil)
		if sendErr != nil {
			logger.Warn().Err(sendErr).Str("asset", guid).Msg("Failed to send asset to album room")
			failed++
		}
	}

	var summary string
	total := len(assetGUIDs)
	if failed == 0 {
		summary = fmt.Sprintf("Downloaded all %d asset(s) from **%s**.", total, albumName)
	} else {
		summary = fmt.Sprintf("Downloaded %d of %d asset(s) from **%s** (%d failed).", total-failed, total, albumName, failed)
	}

	notice := format.RenderMarkdown(summary, true, false)
	notice.MsgType = event.MsgNotice
	_, _ = intent.SendMessage(ctx, roomID, event.EventMessage, &event.Content{
		Parsed: notice,
	}, nil)
}

// safeSharedAlbumDownload wraps the FFI download with panic recovery and a 90s timeout.
func safeSharedAlbumDownload(ss *rustpushgo.WrappedSharedStreamsClient, albumGUID, assetGUID string) ([]byte, error) {
	type dlResult struct {
		data []byte
		err  error
	}
	ch := make(chan dlResult, 1)
	go func() {
		var res dlResult
		defer func() {
			if r := recover(); r != nil {
				stack := string(debug.Stack())
				log.Error().Str("ffi_method", "SharedAlbumDownloadFile").
					Str("album", albumGUID).
					Str("asset", assetGUID).
					Str("stack", stack).
					Msgf("FFI panic recovered: %v", r)
				res = dlResult{err: fmt.Errorf("FFI panic in SharedAlbumDownloadFile: %v", r)}
			}
			ch <- res
		}()
		d, e := ss.DownloadFile(albumGUID, assetGUID)
		res = dlResult{data: d, err: e}
	}()
	select {
	case res := <-ch:
		return res.data, res.err
	case <-time.After(90 * time.Second):
		log.Error().Str("ffi_method", "SharedAlbumDownloadFile").
			Str("album", albumGUID).
			Str("asset", assetGUID).
			Msg("SharedAlbumDownloadFile timed out after 90s")
		return nil, fmt.Errorf("SharedAlbumDownloadFile timed out after 90s")
	}
}

// processSharedAlbumAsset runs the bridge's standard media pipeline on raw
// downloaded bytes: HEIC→JPEG, video transcoding, thumbnail generation, and
// Matrix upload.
func (c *IMClient) processSharedAlbumAsset(ctx context.Context, logger zerolog.Logger, intent interface {
	UploadMedia(ctx context.Context, roomID id.RoomID, data []byte, fileName, mimeType string) (id.ContentURIString, *event.EncryptedFileInfo, error)
}, data []byte, fileName string) *event.MessageEventContent {
	if fileName == "" {
		fileName = "attachment"
	}

	// Infer MIME from filename extension
	mimeType := extensionToMIME(fileName)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	// Audio conversion: CAF → OGG Opus
	var durationMs int
	if mimeType == "audio/x-caf" {
		data, mimeType, fileName, durationMs = convertAudioForMatrix(data, mimeType, fileName)
	}

	// Video transcoding: non-MP4 → MP4
	if c.Main.Config.VideoTranscoding && ffmpeg.Supported() && strings.HasPrefix(mimeType, "video/") && mimeType != "video/mp4" {
		origMime := mimeType
		method := "remux"
		converted, convertErr := ffmpeg.ConvertBytes(ctx, data, ".mp4", nil,
			[]string{"-c", "copy", "-movflags", "+faststart"}, mimeType)
		if convertErr != nil {
			method = "re-encode"
			converted, convertErr = ffmpeg.ConvertBytes(ctx, data, ".mp4", nil,
				[]string{"-c:v", "libx264", "-preset", "fast", "-crf", "23",
					"-c:a", "aac", "-movflags", "+faststart"}, mimeType)
		}
		if convertErr != nil {
			logger.Warn().Err(convertErr).Str("original_mime", origMime).Msg("FFmpeg video conversion failed, uploading original")
		} else {
			logger.Info().Str("original_mime", origMime).Str("method", method).Msg("Video transcoded to MP4")
			data = converted
			mimeType = "video/mp4"
			fileName = strings.TrimSuffix(fileName, filepath.Ext(fileName)) + ".mp4"
		}
	}

	// HEIC → JPEG
	var heicImg image.Image
	data, mimeType, fileName, heicImg = maybeConvertHEIC(&logger, data, mimeType, fileName, c.Main.Config.HEICJPEGQuality, c.Main.Config.HEICConversion)

	// Image dimension extraction and thumbnail generation
	var imgWidth, imgHeight int
	var thumbData []byte
	var thumbW, thumbH int
	if heicImg != nil {
		b := heicImg.Bounds()
		imgWidth, imgHeight = b.Dx(), b.Dy()
		if imgWidth > 800 || imgHeight > 800 {
			thumbData, thumbW, thumbH = scaleAndEncodeThumb(heicImg, imgWidth, imgHeight)
		}
	} else if strings.HasPrefix(mimeType, "image/") || looksLikeImage(data) {
		if mimeType == "image/gif" {
			if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
				imgWidth, imgHeight = cfg.Width, cfg.Height
			}
		} else if img, fmtName, _ := decodeImageData(data); img != nil {
			b := img.Bounds()
			imgWidth, imgHeight = b.Dx(), b.Dy()
			if fmtName == "tiff" {
				var buf bytes.Buffer
				if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err == nil {
					data = buf.Bytes()
					mimeType = "image/jpeg"
					fileName = strings.TrimSuffix(fileName, filepath.Ext(fileName)) + ".jpg"
				}
			}
			if imgWidth > 800 || imgHeight > 800 {
				thumbData, thumbW, thumbH = scaleAndEncodeThumb(img, imgWidth, imgHeight)
			}
		}
	}

	msgType := mimeToMsgType(mimeType)
	content := &event.MessageEventContent{
		MsgType: msgType,
		Body:    fileName,
		Info: &event.FileInfo{
			MimeType: mimeType,
			Size:     len(data),
			Width:    imgWidth,
			Height:   imgHeight,
		},
	}

	if durationMs > 0 {
		content.MSC3245Voice = &event.MSC3245Voice{}
		content.MSC1767Audio = &event.MSC1767Audio{
			Duration: durationMs,
		}
	}

	url, encFile, uploadErr := intent.UploadMedia(ctx, "", data, fileName, mimeType)
	if uploadErr != nil {
		logger.Warn().Err(uploadErr).Str("filename", fileName).Msg("Failed to upload shared album asset to Matrix")
		return nil
	}
	if encFile != nil {
		content.File = encFile
	} else {
		content.URL = url
	}

	if thumbData != nil {
		thumbURL, thumbEnc, thumbErr := intent.UploadMedia(ctx, "", thumbData, "thumbnail.jpg", "image/jpeg")
		if thumbErr != nil {
			logger.Warn().Err(thumbErr).Msg("Failed to upload asset thumbnail")
		} else {
			if thumbEnc != nil {
				content.Info.ThumbnailFile = thumbEnc
			} else {
				content.Info.ThumbnailURL = thumbURL
			}
			content.Info.ThumbnailInfo = &event.FileInfo{
				MimeType: "image/jpeg",
				Size:     len(thumbData),
				Width:    thumbW,
				Height:   thumbH,
			}
		}
	}

	return content
}

// extensionToMIME maps common file extensions to MIME types. Falls back to
// empty string for unknown extensions.
func extensionToMIME(fileName string) string {
	ext := strings.ToLower(filepath.Ext(fileName))
	switch ext {
	case ".heic", ".heif":
		return "image/heic"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".tiff", ".tif":
		return "image/tiff"
	case ".webp":
		return "image/webp"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".avi":
		return "video/x-msvideo"
	case ".caf":
		return "audio/x-caf"
	case ".m4a":
		return "audio/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".aac":
		return "audio/aac"
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// Watcher (notify-only, unchanged behaviour)
// ---------------------------------------------------------------------------

// startSharedStreamsWatcher polls GetChanges() every 10 minutes and posts a
// notice to the management room when subscribed albums receive new content.
// Must be called as a goroutine; exits when c.stopChan is closed.
func (c *IMClient) startSharedStreamsWatcher(log zerolog.Logger) {
	// Wait 2 minutes before first poll so the bridge finishes initialising.
	select {
	case <-c.stopChan:
		return
	case <-time.After(2 * time.Minute):
	}

	if err := c.pollSharedStreams(log); err != nil {
		log.Warn().Err(err).Msg("Initial shared streams poll failed")
	}

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopChan:
			return
		case <-ticker.C:
			if err := c.pollSharedStreams(log); err != nil {
				log.Warn().Err(err).Msg("Shared streams poll failed")
			}
		}
	}
}

// pollSharedStreams calls GetChanges and posts a management-room notice for
// each batch of changed albums. Returns the first error encountered, if any.
func (c *IMClient) pollSharedStreams(log zerolog.Logger) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("shared streams panic: %v", r)
		}
	}()

	if c.client == nil {
		return nil
	}

	ss, err := c.client.GetSharedstreamsClient()
	if err != nil {
		return fmt.Errorf("get shared streams client: %w", err)
	}

	changedAlbums, err := ss.GetChanges()
	if err != nil {
		return fmt.Errorf("get changes: %w", err)
	}

	if len(changedAlbums) == 0 {
		return nil
	}

	notifyAlbums := make([]string, 0, len(changedAlbums))
	for _, albumID := range changedAlbums {
		assets, summaryErr := ss.GetAlbumSummary(albumID)
		if summaryErr != nil {
			return fmt.Errorf("get album summary for %s: %w", albumID, summaryErr)
		}
		if c.recordSharedStreamAssets(albumID, assets) {
			notifyAlbums = append(notifyAlbums, albumID)
		}
	}

	if len(notifyAlbums) == 0 {
		log.Debug().Strs("albums", changedAlbums).Msg("Shared albums changed, but no new visible assets were added")
		return nil
	}

	log.Info().Strs("albums", notifyAlbums).Msg("Detected new content in shared albums")

	ctx := context.Background()
	mgmtRoom, err := c.UserLogin.User.GetManagementRoom(ctx)
	if err != nil {
		return fmt.Errorf("get management room: %w", err)
	}

	var sb strings.Builder
	if len(notifyAlbums) == 1 {
		sb.WriteString(fmt.Sprintf("\U0001f4f8 New content in shared album `%s`.", notifyAlbums[0]))
	} else {
		sb.WriteString(fmt.Sprintf("\U0001f4f8 New content in %d shared albums:\n\n", len(notifyAlbums)))
		for _, id := range notifyAlbums {
			sb.WriteString(fmt.Sprintf("- `%s`\n", id))
		}
	}
	sb.WriteString("\n\nUse `shared-albums` to browse and download.")

	content := format.RenderMarkdown(sb.String(), true, false)
	content.MsgType = event.MsgNotice
	_, sendErr := c.Main.Bridge.Bot.SendMessage(ctx, mgmtRoom, event.EventMessage, &event.Content{
		Parsed: content,
	}, nil)
	if sendErr != nil {
		return fmt.Errorf("send shared streams notification: %w", sendErr)
	}

	return nil
}

func (c *IMClient) recordSharedStreamAssets(albumID string, assets []string) bool {
	current := make(map[string]struct{}, len(assets))
	for _, assetID := range assets {
		if assetID != "" {
			current[assetID] = struct{}{}
		}
	}

	c.sharedStreamAssetCacheMu.Lock()
	defer c.sharedStreamAssetCacheMu.Unlock()

	previous, hadBaseline := c.sharedStreamAssetCache[albumID]
	c.sharedStreamAssetCache[albumID] = current

	if !hadBaseline {
		return false
	}
	for assetID := range current {
		if _, seen := previous[assetID]; !seen {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Helpers shared across commands
// ---------------------------------------------------------------------------

func sharedStreamsClientFromEvent(ce *commands.Event) (*rustpushgo.WrappedSharedStreamsClient, bool) {
	login := ce.User.GetDefaultLogin()
	if login == nil {
		ce.Reply("No active login found.")
		return nil, false
	}
	client, ok := login.Client.(*IMClient)
	if !ok || client == nil || client.client == nil {
		ce.Reply("Bridge client not available.")
		return nil, false
	}
	ss, err := client.client.GetSharedstreamsClient()
	if err != nil {
		ce.Reply("Failed to initialize shared streams client: %v", err)
		return nil, false
	}
	return ss, true
}

func fnSharedSubscribe(ce *commands.Event) {
	if len(ce.Args) < 1 {
		ce.Reply("Usage: `!shared-subscribe <album-id>`")
		return
	}
	ss, ok := sharedStreamsClientFromEvent(ce)
	if !ok {
		return
	}
	albumID := strings.TrimSpace(ce.Args[0])
	if err := ss.Subscribe(albumID); err != nil {
		ce.Reply("Failed to subscribe to album `%s`: %v", albumID, err)
		return
	}
	ce.Reply("Subscribed to shared album `%s`.", albumID)
}

func fnSharedSubscribeToken(ce *commands.Event) {
	if len(ce.Args) < 1 {
		ce.Reply("Usage: `!shared-subscribe-token <token>`")
		return
	}
	ss, ok := sharedStreamsClientFromEvent(ce)
	if !ok {
		return
	}
	token := strings.TrimSpace(ce.Args[0])
	if err := ss.SubscribeToken(token); err != nil {
		ce.Reply("Failed to subscribe using token: %v", err)
		return
	}
	ce.Reply("Subscribed to shared album using token.")
}

func fnSharedUnsubscribe(ce *commands.Event) {
	if len(ce.Args) < 1 {
		ce.Reply("Usage: `!shared-unsubscribe <album-id>`")
		return
	}
	ss, ok := sharedStreamsClientFromEvent(ce)
	if !ok {
		return
	}
	albumID := strings.TrimSpace(ce.Args[0])
	if err := ss.Unsubscribe(albumID); err != nil {
		ce.Reply("Failed to unsubscribe from album `%s`: %v", albumID, err)
		return
	}
	ce.Reply("Unsubscribed from shared album `%s`.", albumID)
}

func fnSharedState(ce *commands.Event) {
	ss, ok := sharedStreamsClientFromEvent(ce)
	if !ok {
		return
	}
	state, err := ss.ExportStateJson()
	if err != nil {
		ce.Reply("Failed to export shared streams state: %v", err)
		return
	}
	if len(state) > 12000 {
		state = state[:12000] + "\n... (truncated)"
	}
	ce.Reply("```json\n%s\n```", state)
}

func fnSharedAssetsJSON(ce *commands.Event) {
	if len(ce.Args) < 2 {
		ce.Reply("Usage: `!shared-assets-json <album-id> <asset-guid...>`")
		return
	}
	ss, ok := sharedStreamsClientFromEvent(ce)
	if !ok {
		return
	}
	albumID := strings.TrimSpace(ce.Args[0])
	assets := parseListArgs(ce.Args[1:])
	if len(assets) == 0 {
		ce.Reply("Please provide at least one asset GUID.")
		return
	}
	assetsJSON, err := ss.GetAssetsJson(albumID, assets)
	if err != nil {
		ce.Reply("Failed to fetch assets JSON: %v", err)
		return
	}
	if len(assetsJSON) > 12000 {
		assetsJSON = assetsJSON[:12000] + "\n... (truncated)"
	}
	ce.Reply("```json\n%s\n```", assetsJSON)
}

func fnSharedDeleteAssets(ce *commands.Event) {
	if len(ce.Args) < 2 {
		ce.Reply("Usage: `!shared-delete-assets <album-id> <asset-guid...>`")
		return
	}
	ss, ok := sharedStreamsClientFromEvent(ce)
	if !ok {
		return
	}
	albumID := strings.TrimSpace(ce.Args[0])
	assets := parseListArgs(ce.Args[1:])
	if len(assets) == 0 {
		ce.Reply("Please provide at least one asset GUID.")
		return
	}
	if err := ss.DeleteAssets(albumID, assets); err != nil {
		ce.Reply("Failed to delete assets: %v", err)
		return
	}
	ce.Reply("Deleted %d asset(s) from `%s`.", len(assets), albumID)
}
