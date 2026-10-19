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

package imessage

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	TapbackLove TapbackType = iota + 2000
	TapbackLike
	TapbackDislike
	TapbackLaugh
	TapbackEmphasis
	TapbackQuestion
	TapbackCustom

	TapbackRemoveOffset = 1000
)

type Tapback struct {
	TargetGUID  string      `json:"target_guid"`
	TargetPart  int         `json:"-"`
	Remove      bool        `json:"-"`
	Type        TapbackType `json:"type"`
	CustomEmoji string      `json:"emoji,omitempty"`
}

var (
	ErrUnknownNormalTapbackTarget = errors.New("unrecognized formatting of normal tapback target")
	ErrInvalidTapbackTargetPart   = errors.New("tapback target part index is invalid")
	ErrUnknownTapbackTargetType   = errors.New("unrecognized tapback target type")
)

func (tapback *Tapback) Parse() (*Tapback, error) {
	if tapback.Type >= 3000 && tapback.Type < 4000 {
		tapback.Type -= TapbackRemoveOffset
		tapback.Remove = true
	}
	if strings.HasPrefix(tapback.TargetGUID, "bp:") {
		tapback.TargetGUID = tapback.TargetGUID[len("bp:"):]
	} else if strings.HasPrefix(tapback.TargetGUID, "p:") {
		targetParts := strings.Split(tapback.TargetGUID[len("p:"):], "/")
		if len(targetParts) == 2 {
			var err error
			tapback.TargetPart, err = strconv.Atoi(targetParts[0])
			if err != nil {
				return nil, fmt.Errorf("%w: '%s' (%v)", ErrInvalidTapbackTargetPart, tapback.TargetGUID, err)
			}
			tapback.TargetGUID = targetParts[1]
		} else {
			return nil, fmt.Errorf("%w: '%s'", ErrUnknownNormalTapbackTarget, tapback.TargetGUID)
		}
	} else {
		return nil, fmt.Errorf("%w: '%s'", ErrUnknownTapbackTargetType, tapback.TargetGUID)
	}
	return tapback, nil
}

type TapbackType int

func TapbackFromEmoji(emoji string) TapbackType {
	if emoji == "" {
		return 0
	}
	switch []rune(emoji)[0] {
	case '\u2665', '\u2764', '\U0001f499', '\U0001f49a', '\U0001f90e', '\U0001f5a4', '\U0001f90d', '\U0001f9e1',
		'\U0001f49b', '\U0001f49c', '\U0001f496', '\u2763', '\U0001f495', '\U0001f49f':
		// '♥', '❤', '💙', '💚', '🤎', '🖤', '🤍', '🧡', '💛', '💜', '💖', '❣', '💕', '💟'
		return TapbackLove
	case '\U0001f44d': // '👍'
		return TapbackLike
	case '\U0001f44e': // '👎'
		return TapbackDislike
	case '\U0001f602', '\U0001f639', '\U0001f606', '\U0001f923': // '😂', '😹', '😆', '🤣'
		return TapbackLaugh
	case '\u2755', '\u2757', '\u203c': // '❕', '❗', '‼',
		return TapbackEmphasis
	case '\u2753', '\u2754': // '❓', '❔'
		return TapbackQuestion
	default:
		return 0
	}
}

func TapbackFromName(name string) TapbackType {
	switch name {
	case "love":
		return TapbackLove
	case "like":
		return TapbackLike
	case "dislike":
		return TapbackDislike
	case "laugh":
		return TapbackLaugh
	case "emphasize":
		return TapbackEmphasis
	case "question":
		return TapbackQuestion
	default:
		return 0
	}
}

func (amt TapbackType) String() string {
	return amt.Emoji()
}

func (amt TapbackType) Emoji() string {
	switch amt {
	case 0:
		return ""
	case TapbackLove:
		return "\u2764\ufe0f" // "❤️"
	case TapbackLike:
		return "\U0001f44d\ufe0f" // "👍️"
	case TapbackDislike:
		return "\U0001f44e\ufe0f" // "👎️"
	case TapbackLaugh:
		return "\U0001f602" // "😂"
	case TapbackEmphasis:
		return "\u203c\ufe0f" // "‼️"
	case TapbackQuestion:
		return "\u2753\ufe0f" // "❓️"
	default:
		return "\ufffd" // "�"
	}
}

func (amt TapbackType) Name() string {
	switch amt {
	case 0:
		return ""
	case TapbackLove:
		return "love"
	case TapbackLike:
		return "like"
	case TapbackDislike:
		return "dislike"
	case TapbackLaugh:
		return "laugh"
	case TapbackEmphasis:
		return "emphasize"
	case TapbackQuestion:
		return "question"
	default:
		return ""
	}
}

// Emoji returns the original Unicode sequence for a custom Tapback.
func (tapback *Tapback) Emoji() string {
	if tapback.Type == TapbackCustom {
		return tapback.CustomEmoji
	}
	return tapback.Type.Emoji()
}

// EmojiTapbackAPI is optional: existing connectors keep their classic behavior.
type EmojiTapbackAPI interface {
	SupportsEmojiTapbacks() bool
	SendEmojiTapback(chatID, targetGUID string, targetPart int, emoji string, remove bool) (*SendResponse, error)
}

// ReactionFromEmoji preserves arbitrary emoji only on an explicitly capable connector.
func ReactionFromEmoji(emoji string, custom bool) (TapbackType, string) {
	if emoji == "" {
		return 0, ""
	}
	if !custom {
		return TapbackFromEmoji(emoji), ""
	}
	switch emoji {
	case "❤", "❤️":
		return TapbackLove, ""
	case "👍", "👍️":
		return TapbackLike, ""
	case "👎", "👎️":
		return TapbackDislike, ""
	case "😂":
		return TapbackLaugh, ""
	case "‼", "‼️":
		return TapbackEmphasis, ""
	case "❓", "❓️":
		return TapbackQuestion, ""
	default:
		return TapbackCustom, emoji
	}
}

func SupportsEmojiTapbacks(api API) bool {
	sender, ok := api.(EmojiTapbackAPI)
	return ok && sender.SupportsEmojiTapbacks()
}

func SendReaction(api API, chatID, targetGUID string, targetPart int, kind TapbackType, emoji string, remove bool) (*SendResponse, error) {
	if kind == TapbackCustom {
		sender, ok := api.(EmojiTapbackAPI)
		if !ok || !sender.SupportsEmojiTapbacks() || emoji == "" {
			return nil, errors.New("custom emoji reactions require a compatible BlueBubbles server and macOS Tahoe 26 or newer")
		}
		return sender.SendEmojiTapback(chatID, targetGUID, targetPart, emoji, remove)
	}
	return api.SendTapback(chatID, targetGUID, targetPart, kind, remove)
}
