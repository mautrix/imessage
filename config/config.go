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

package config

import (
	"os"

	"gopkg.in/yaml.v2"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/imessage"
)

type Config struct {
	Homeserver struct {
		Address    string `yaml:"address"`
		WSProxy    string `yaml:"websocket_proxy"`
		Domain     string `yaml:"domain"`
		Asmux      bool   `yaml:"asmux"`
		AsyncMedia bool   `yaml:"async_media"`

		PingInterval int `yaml:"ping_interval_seconds"`
	} `yaml:"homeserver"`

	AppService struct {
		Database string `yaml:"database"`

		ID  string `yaml:"id"`
		Bot struct {
			Username    string `yaml:"username"`
			Displayname string `yaml:"displayname"`
			Avatar      string `yaml:"avatar"`

			ParsedAvatar id.ContentURI `yaml:"-"`
		} `yaml:"bot"`

		ASToken string `yaml:"as_token"`
		HSToken string `yaml:"hs_token"`

		EphemeralEvents bool `yaml:"ephemeral_events"`
	} `yaml:"appservice"`

	IMessage imessage.PlatformConfig `yaml:"imessage"`

	Bridge BridgeConfig `yaml:"bridge"`

	Logging appservice.LogConfig `yaml:"logging"`
}

func (config *Config) setDefaults() {
	config.IMessage.PingInterval = 15
	config.Bridge.setDefaults()
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config = &Config{}
	config.setDefaults()
	err = yaml.Unmarshal(data, config)
	if len(config.Bridge.DoublePuppetServerURL) == 0 {
		config.Bridge.DoublePuppetServerURL = config.Homeserver.Address
	}
	return config, err
}

func (config *Config) Save(path string) error {
	data, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func (config *Config) MakeAppService() (*appservice.AppService, error) {
	as := appservice.Create()
	as.HomeserverDomain = config.Homeserver.Domain
	as.HomeserverURL = config.Homeserver.Address
	as.DefaultHTTPRetries = 4
	var err error
	as.Registration, err = config.GetRegistration()
	return as, err
}
