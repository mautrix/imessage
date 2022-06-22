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
	"bytes"
	"context"
	"encoding/json"
	"image"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/tidwall/gjson"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/imessage"
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
	if len(preview.MatchedURL) == 0 && len(preview.CanonicalURL) == 0 {
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
	} else if output.OriginalURL == "" {
		output.OriginalURL = preview.CanonicalURL
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

func (portal *Portal) convertRichLinkToBeeper(richLink *imessage.RichLink) (output *BeeperLinkPreview) {
	if richLink == nil {
		return
	}
	description := richLink.SelectedText
	if description == "" {
		description = richLink.Summary
	}

	output = &BeeperLinkPreview{
		MatchedURL:   richLink.OriginalURL,
		CanonicalURL: richLink.URL,
		Title:        richLink.Title,
		Description:  description,
	}

	if richLink.Image != nil {
		if richLink.Image.Size != nil {
			output.ImageWidth = int(richLink.Image.Size.Width)
			output.ImageHeight = int(richLink.Image.Size.Height)
		}
		output.ImageType = richLink.ItemType

		if richLink.Image.Source != nil {
			thumbnailData := richLink.Image.Source.Data
			if thumbnailData == nil {
				if url := richLink.Image.Source.URL; url != "" {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
					defer cancel()
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
					if err != nil {
						return
					}
					thumbnailData, _ = ioutil.ReadAll(req.Body)
				}
			}

			if output.ImageHeight == 0 || output.ImageWidth == 0 {
				src, _, err := image.Decode(bytes.NewReader(thumbnailData))
				if err == nil {
					imageBounds := src.Bounds()
					output.ImageWidth, output.ImageHeight = imageBounds.Max.X, imageBounds.Max.Y
				}
			}

			output.ImageSize = len(thumbnailData)
			if output.ImageType == "" {
				output.ImageType = http.DetectContentType(thumbnailData)
			}

			uploadData, uploadMime := thumbnailData, output.ImageType
			if portal.Encrypted {
				crypto := attachment.NewEncryptedFile()
				crypto.EncryptInPlace(uploadData)
				uploadMime = "application/octet-stream"
				output.ImageEncryption = &event.EncryptedFileInfo{EncryptedFile: *crypto}
			}
			resp, err := portal.bridge.Bot.UploadBytes(uploadData, uploadMime)
			if err != nil {
				portal.log.Warnfln("Failed to reupload thumbnail for link preview: %v", err)
			} else {
				if output.ImageEncryption != nil {
					output.ImageEncryption.URL = resp.ContentURI.CUString()
				} else {
					output.ImageURL = resp.ContentURI.CUString()
				}
			}
		}

	}

	return
}
