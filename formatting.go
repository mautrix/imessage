// mautrix-IMessage - A Matrix-IMessage puppeting bridge.
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
	"fmt"
	"regexp"

	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"
)

var italicRegex = regexp.MustCompile("([\\s>~*]|^)_(.+?)_([^a-zA-Z\\d]|$)")
var boldRegex = regexp.MustCompile("([\\s>_~]|^)\\*(.+?)\\*([^a-zA-Z\\d]|$)")
var strikethroughRegex = regexp.MustCompile("([\\s>_*]|^)~(.+?)~([^a-zA-Z\\d]|$)")
var codeBlockRegex = regexp.MustCompile("```(?:.|\n)+?```")

const mentionedName = "mentioned_user"

type Formatter struct {
	bridge *Bridge

	matrixHTMLParser *format.HTMLParser
}

func NewFormatter(bridge *Bridge) *Formatter {
	formatter := &Formatter{
		bridge: bridge,
		matrixHTMLParser: &format.HTMLParser{
			TabsToSpaces: 4,
			Newline:      "\n",

			PillConverter: func(displayname, mxid, eventID string, ctx format.Context) string {
				if mxid[0] == '@' {
					puppet := bridge.GetPuppetByMXID(id.UserID(mxid))
					if puppet != nil {
						user, ok := ctx[mentionedName].([]string)
						if !ok {
							ctx[mentionedName] = []string{puppet.ID}
						} else {
							ctx[mentionedName] = append(user, puppet.ID)
						}
						return "@" + puppet.Displayname
					}
				}
				return "@" + displayname
			},
			BoldConverter:           func(text string, _ format.Context) string { return fmt.Sprintf("*%s*", text) },
			ItalicConverter:         func(text string, _ format.Context) string { return fmt.Sprintf("_%s_", text) },
			StrikethroughConverter:  func(text string, _ format.Context) string { return fmt.Sprintf("~%s~", text) },
			MonospaceConverter:      func(text string, _ format.Context) string { return fmt.Sprintf("```%s```", text) },
			MonospaceBlockConverter: func(text, language string, _ format.Context) string { return fmt.Sprintf("```%s```", text) },
		},
	}
	return formatter
}
func (formatter *Formatter) ParseMatrix(html string) (string, []string) {
	ctx := make(format.Context)
	result := formatter.matrixHTMLParser.Parse(html, ctx)
	mentionedJIDs, _ := ctx[mentionedName].([]string)
	return result, mentionedJIDs
}
