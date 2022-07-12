// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2022 Tulir Asokan
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
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"

	"golang.org/x/crypto/hkdf"

	"maunium.net/go/mautrix/event"
)

type mediaViewerCreateRequest struct {
	Ciphertext string `json:"ciphertext"`
	AuthToken  string `json:"auth_token"`
	Homeserver string `json:"homeserver"`
}

type mediaViewerCreateResponse struct {
	Error  string `json:"message"`
	FileID string `json:"file_id"`
}

func extractKeys(mediaKey []byte) (encryption, iv, auth []byte, err error) {
	prk := hkdf.Extract(sha512.New, mediaKey, nil)
	encryption = make([]byte, 32)
	iv = make([]byte, 12)
	auth = make([]byte, 32)

	if _, err = io.ReadFull(hkdf.Expand(sha512.New, prk, []byte("encryption")), encryption); err != nil {
		err = fmt.Errorf("encryption hkdf failed: %w", err)
	} else if _, err = io.ReadFull(hkdf.Expand(sha512.New, prk, []byte("initialization")), iv); err != nil {
		err = fmt.Errorf("iv hkdf failed: %w", err)
	} else if _, err = io.ReadFull(hkdf.Expand(sha512.New, prk, []byte("authentication")), auth); err != nil {
		err = fmt.Errorf("authentication hkdf failed: %w", err)
	}
	return
}

func (br *IMBridge) createMediaViewerURL(content *event.Content) (string, error) {
	msg := content.AsMessage()
	if msg.File == nil {
		if len(msg.URL) > 0 {
			parsedMXC, err := msg.URL.Parse()
			return br.Bot.GetDownloadURL(parsedMXC), err
		}
		return "", fmt.Errorf("no URL in message")
	}

	parsedURL, err := url.Parse(br.Config.Bridge.MediaViewer.URL)
	if err != nil {
		return "", fmt.Errorf("invalid media viewer URL in config: %w", err)
	}
	origPath := parsedURL.Path
	parsedURL.Path = path.Join(origPath, "create")
	createURL := parsedURL.String()

	mediaKey := make([]byte, 16)
	var encryptionKey, iv, authToken []byte
	var ciphertext []byte
	if _, err = rand.Read(mediaKey); err != nil {
		return "", fmt.Errorf("failed to generate media key: %w", err)
	} else if encryptionKey, iv, authToken, err = extractKeys(mediaKey); err != nil {
		return "", err
	} else if block, err := aes.NewCipher(encryptionKey); err != nil {
		return "", fmt.Errorf("failed to prepare AES cipher: %w", err)
	} else if gcm, err := cipher.NewGCM(block); err != nil {
		return "", fmt.Errorf("failed to prepare GCM cipher: %w", err)
	} else {
		ciphertext = gcm.Seal(nil, iv, content.VeryRaw, nil)
	}

	var reqDataBytes bytes.Buffer
	reqData := mediaViewerCreateRequest{
		Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext),
		AuthToken:  base64.RawStdEncoding.EncodeToString(authToken),
		Homeserver: br.Config.Homeserver.Domain,
	}
	var respData mediaViewerCreateResponse

	if err = json.NewEncoder(&reqDataBytes).Encode(&reqData); err != nil {
		return "", fmt.Errorf("failed to marshal create request: %w", err)
	} else if req, err := http.NewRequest(http.MethodPost, createURL, &reqDataBytes); err != nil {
		return "", fmt.Errorf("failed to prepare create request: %w", err)
	} else if resp, err := http.DefaultClient.Do(req); err != nil {
		return "", fmt.Errorf("failed to send create request: %w", err)
	} else if err = json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		if resp.StatusCode >= 400 {
			return "", fmt.Errorf("server returned non-JSON error with status code %d", resp.StatusCode)
		}
		return "", fmt.Errorf("failed to decode response: %w", err)
	} else if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, respData.Error)
	} else {
		parsedURL.Path = path.Join(origPath, respData.FileID)
		parsedURL.Fragment = base64.RawURLEncoding.EncodeToString(mediaKey)
		return parsedURL.String(), nil
	}
}
