package imessage

import "testing"

func TestReactionPreservesUnicode(t *testing.T) {
	for _, emoji := range []string{"💙", "🤣", "👍🏽", "👩🏽‍💻", "🇺🇸", "1️⃣", "🫩"} {
		kind, custom := ReactionFromEmoji(emoji, true)
		if kind != TapbackCustom || custom != emoji {
			t.Fatalf("%q became %d %q", emoji, kind, custom)
		}
		tb := &Tapback{Type: kind, CustomEmoji: custom}
		if tb.Emoji() != emoji {
			t.Fatalf("lost emoji %q", emoji)
		}
	}
	for _, emoji := range []string{"❤️", "👍", "👎", "😂", "‼️", "❓"} {
		kind, custom := ReactionFromEmoji(emoji, true)
		if kind < TapbackLove || kind > TapbackQuestion || custom != "" {
			t.Fatalf("classic %q became custom", emoji)
		}
	}
	if kind, _ := ReactionFromEmoji("", true); kind != 0 {
		t.Fatal("empty reaction accepted")
	}
	if TapbackFromEmoji("") != 0 {
		t.Fatal("empty legacy reaction accepted")
	}
	if kind, custom := ReactionFromEmoji("💙", false); kind != TapbackLove || custom != "" {
		t.Fatal("legacy behavior changed")
	}
}

type reactionAPI struct {
	API
	enabled                   bool
	classicCalls, customCalls int
	emoji                     string
	remove                    bool
}

func (a *reactionAPI) SupportsEmojiTapbacks() bool { return a.enabled }
func (a *reactionAPI) SendTapback(_, _ string, _ int, _ TapbackType, _ bool) (*SendResponse, error) {
	a.classicCalls++
	return &SendResponse{}, nil
}
func (a *reactionAPI) SendEmojiTapback(_, _ string, _ int, emoji string, remove bool) (*SendResponse, error) {
	a.customCalls++
	a.emoji = emoji
	a.remove = remove
	return &SendResponse{}, nil
}
func TestReactionDispatch(t *testing.T) {
	a := &reactionAPI{enabled: true}
	if _, err := SendReaction(a, "chat", "target", 2, TapbackCustom, "👩🏽‍💻", true); err != nil {
		t.Fatal(err)
	}
	if a.customCalls != 1 || a.emoji != "👩🏽‍💻" || !a.remove {
		t.Fatal("custom removal lost data")
	}
	if _, err := SendReaction(a, "chat", "target", 2, TapbackLove, "", false); err != nil || a.classicCalls != 1 {
		t.Fatal("classic dispatch failed")
	}
	a.enabled = false
	if _, err := SendReaction(a, "chat", "target", 2, TapbackCustom, "💙", false); err == nil || a.customCalls != 1 {
		t.Fatal("unsupported connector accepted custom emoji")
	}
}
