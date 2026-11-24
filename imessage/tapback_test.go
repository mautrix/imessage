package imessage

import "testing"

func TestTapbackFromEmojiEmptyDoesNotPanic(t *testing.T) {
	if got := TapbackFromEmoji(""); got != 0 {
		t.Fatalf("expected 0 for empty emoji, got %d", got)
	}
	if got := TapbackFromEmoji("❤️"); got != TapbackLove {
		t.Fatalf("expected TapbackLove, got %d", got)
	}
	if got := TapbackFromEmoji("🔥"); got != 0 {
		t.Fatalf("expected 0 for custom emoji, got %d", got)
	}
}

func TestTapbackCustomEmoji(t *testing.T) {
	tb := &Tapback{
		TargetGUID:  "p:1/MSG-123",
		Type:        TapbackCustomEmoji,
		CustomEmoji: "🔥",
	}
	if _, err := tb.Parse(); err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if tb.TargetGUID != "MSG-123" || tb.TargetPart != 1 || tb.Remove {
		t.Fatalf("unexpected parsed tapback: %+v", tb)
	}
	if got := tb.Emoji(); got != "🔥" {
		t.Fatalf("expected 🔥, got %q", got)
	}

	rm := &Tapback{
		TargetGUID:  "bp:MSG-456",
		Type:        TapbackCustomEmoji + TapbackRemoveOffset,
		CustomEmoji: "🔥",
	}
	if _, err := rm.Parse(); err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if !rm.Remove || rm.Type != TapbackCustomEmoji || rm.TargetGUID != "MSG-456" {
		t.Fatalf("unexpected parsed removal tapback: %+v", rm)
	}
}
