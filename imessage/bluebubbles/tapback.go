package bluebubbles

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.mau.fi/mautrix-imessage/imessage"
)

func (bb *blueBubbles) SupportsEmojiTapbacks() bool {
	major, err := strconv.Atoi(strings.Split(bb.serverOSVersion, ".")[0])
	return err == nil && major >= 26 && bb.serverCustomEmojiReactions && bb.usingPrivateAPI && bb.bridge.GetConnectorConfig().BlueBubblesCustomEmojiReactions
}

func (bb *blueBubbles) SendEmojiTapback(chatID, targetGUID string, targetPart int, emoji string, remove bool) (*imessage.SendResponse, error) {
	if !bb.SupportsEmojiTapbacks() {
		return nil, fmt.Errorf("custom emoji reactions require opt-in, macOS Tahoe 26+, and a compatible BlueBubbles private API")
	}
	if !validCustomEmoji(emoji) {
		return nil, fmt.Errorf("invalid custom emoji reaction")
	}
	return bb.sendReaction(chatID, targetGUID, targetPart, emoji, remove)
}

// BlueBubbles's custom API uses the Unicode sequence as reaction, and a leading
// '-' for removal (BlueBubblesApp/bluebubbles-server PR #839).
func validCustomEmoji(emoji string) bool {
	if emoji == "" || len(emoji) > 256 || !utf8.ValidString(emoji) || strings.ContainsAny(emoji, " \t\r\n") || strings.HasPrefix(emoji, "-") {
		return false
	}
	// Reject classic names, URLs, and arbitrary ASCII reaction keys. Keycaps,
	// flags, skin tones and ZWJ sequences all contain non-ASCII codepoints.
	for _, r := range emoji {
		if r > 127 {
			return true
		}
	}
	return false
}

func parseBBTapback(target, kind, emoji string) (*imessage.Tapback, error) {
	remove := strings.HasPrefix(kind, "-")
	name := strings.TrimPrefix(kind, "-")
	reaction := imessage.TapbackFromName(name)
	if reaction == 0 {
		switch name {
		case "2006", "3006", "emoji":
			remove = remove || name == "3006"
		default:
			// Some BlueBubbles builds put the Unicode emoji directly in the type.
			emoji = name
		}
		if !validCustomEmoji(emoji) {
			return nil, fmt.Errorf("unsupported or malformed BlueBubbles reaction %q", kind)
		}
		reaction = imessage.TapbackCustom
	} else {
		emoji = ""
	}
	tb := &imessage.Tapback{TargetGUID: target, Type: reaction, CustomEmoji: emoji, Remove: remove}
	return tb.Parse()
}
