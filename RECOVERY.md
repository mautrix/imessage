# Recover selected BlueBubbles history

Startup catch-up normally starts after the most recent message recorded for each room. Use a recovery plan if you know the source GUIDs of older messages or reactions that missed that checkpoint.

This requires the BlueBubbles connector, enabled backfill, and a homeserver that supports Beeper batch sending. Prepare a private JSON file with the chat GUIDs of existing bridged rooms, a timestamp before the events you want to recover, and their source message or reaction GUIDs. For example

```json
{
  "since": "2025-01-01T00:00:00Z",
  "chats": ["iMessage;+;example-chat"],
  "message_guids": ["example-message", "example-reaction"]
}
```

1. Set `MAUTRIX_IMESSAGE_RECOVERY_PLAN` to the path of the file in the bridge's environment and restart the bridge.
2. Check the bridge logs and confirm the selected events appeared in the existing rooms.
3. Remove the environment variable before the next restart.

Recovery runs during normal startup sync and does not create rooms, delete mappings, or mark the recovered events as read. It skips parts and reactions that already have event mappings. Select only reactions that are still present in Apple Messages. A reaction needs its target part in that room and an unoccupied reaction slot for its sender. Attachments still need to be available from BlueBubbles. An invalid or unreadable plan is logged and normal catch-up continues.

Adding an older reaction can affect room order in Beeper Desktop ([#242](https://github.com/mautrix/imessage/issues/242)).
