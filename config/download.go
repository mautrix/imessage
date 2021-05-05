// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2021 Tulir Asokan
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

package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"maunium.net/go/mautrix"

	"go.mau.fi/mautrix-imessage/ipc"
)

func sanitize(secret string) string {
	parsedURL, _ := url.Parse(secret)
	parsedURL.User = nil
	parsedURL.RawQuery = ""
	return parsedURL.String()
}

type configRedirect struct {
	URL        string `json:"url"`
	Redirected bool   `json:"redirected"`
}

func output(url string, redirected bool) error {
	raw, err := json.Marshal(&ipc.OutgoingMessage{
		Command: "config_url",
		Data: &configRedirect{
			URL:        url,
			Redirected: redirected,
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", raw)
	return nil
}

func getRedirect(configURL string) (string, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(http.MethodHead, configURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", mautrix.DefaultUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to request HEAD %s: %w", sanitize(configURL), err)
	}
	if resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusSeeOther || resp.StatusCode == http.StatusTemporaryRedirect || resp.StatusCode == http.StatusPermanentRedirect {
		var targetURL *url.URL
		targetURL, err = resp.Location()
		if err != nil {
			return "", fmt.Errorf("failed to get redirect target location: %w", err)
		}
		configURL = targetURL.String()
		err = output(configURL, true)
	} else if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HEAD %s returned HTTP %d", sanitize(configURL), resp.StatusCode)
	} else {
		err = output(configURL, false)
	}
	if err != nil {
		return "", fmt.Errorf("failed to output new config URL: %w", err)
	}
	return configURL, nil
}

func Download(configURL, saveTo string, outputRedirect bool) error {
	client := &http.Client{
		Timeout: 30 * time.Second,
	}
	if outputRedirect {
		var err error
		configURL, err = getRedirect(configURL)
		if err != nil {
			return fmt.Errorf("failed to check config redirect: %w", err)
		}
	}
	req, err := http.NewRequest(http.MethodGet, configURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", mautrix.DefaultUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to request GET %s: %w", sanitize(configURL), err)
	} else if resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s returned HTTP %d", sanitize(configURL), resp.StatusCode)
	}
	defer resp.Body.Close()
	file, err := os.OpenFile(saveTo, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to open %s for writing config: %w", saveTo, err)
	}
	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to write config to %s: %w", saveTo, err)
	}
	return nil
}
