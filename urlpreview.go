// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2021 Tulir Asokan, Sumner Evans
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

package main

import (
	"encoding/json"

	"github.com/tidwall/gjson"
	"go.mau.fi/mautrix-imessage/imessage"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type BeeperLinkPreview struct {
	MatchedURL   string `json:"matched_url"`
	CanonicalURL string `json:"og:url,omitempty"`
	Title        string `json:"og:title,omitempty"`
	Type         string `json:"og:type,omitempty"`
	Description  string `json:"og:description,omitempty"`

	ImageURL        id.ContentURIString      `json:"og:image,omitempty"`
	ImageEncryption *event.EncryptedFileInfo `json:"beeper:image:encryption,omitempty"`

	ImageSize   int    `json:"matrix:image:size,omitempty"`
	ImageWidth  int    `json:"og:image:width,omitempty"`
	ImageHeight int    `json:"og:image:height,omitempty"`
	ImageType   string `json:"og:image:type,omitempty"`
}

func (portal *Portal) convertURLPreviewToIMessage(evt *event.Event) (output *imessage.RichLink) {
	rawPreview := gjson.GetBytes(evt.Content.VeryRaw, `com\.beeper\.linkpreviews`)
	if !rawPreview.Exists() || !rawPreview.IsArray() {
		return
	}
	var previews []BeeperLinkPreview
	if err := json.Unmarshal([]byte(rawPreview.Raw), &previews); err != nil || len(previews) == 0 {
		return
	}
	// iMessage only supports a single preview.
	preview := previews[0]
	if len(preview.MatchedURL) == 0 {
		return
	}

	output = &imessage.RichLink{
		OriginalURL: preview.MatchedURL,
		URL:         preview.CanonicalURL,
		Title:       preview.Title,
		Summary:     preview.Description,
		ItemType:    preview.Type,
	}

	if output.URL == "" {
		output.URL = preview.MatchedURL
	}

	if preview.ImageURL != "" || preview.ImageEncryption != nil {
		output.Image = &imessage.RichLinkAsset{
			MimeType: preview.ImageType,
			Size: &imessage.RichLinkAssetSize{
				Width:  float64(preview.ImageWidth),
				Height: float64(preview.ImageHeight),
			},
			Source: &imessage.RichLinkAssetSource{},
		}

		var contentUri id.ContentURI
		var err error
		if preview.ImageURL == "" {
			contentUri, err = preview.ImageEncryption.URL.Parse()
		} else {
			contentUri, err = preview.ImageURL.Parse()
		}
		if err != nil {
			return
		}

		imgBytes, err := portal.bridge.Bot.DownloadBytes(contentUri)
		if err != nil {
			return
		}

		if preview.ImageEncryption != nil {
			err = preview.ImageEncryption.DecryptInPlace(imgBytes)
			if err != nil {
				return
			}
		}
		output.Image.Source.Data = imgBytes
	}
	return
}
