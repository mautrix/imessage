# iMessage bridge protocol

## Setup (when mautrix-imessage is the subprocess)
The bridge needs a config file that has the homeserver details, access tokens
and other such things. Brooklyn needs to get that config file from somewhere
and point the bridge at it when running. The setup UX should just be scanning
a QR code.

1. User scans QR code with Brooklyn on iPhone. The QR code contains a URL,
   which may end in a newline. Strip away the newline if necessary.
2. Start the mautrix-imessage subprocess with `--url <url from QR> --output-redirect`.
   The second flag tells the bridge to follow potential redirects in the URL
   and output the direct URL using the `config_url` IPC command.
3. Save the URL from the output and just pass `--url <saved url>` on future runs.

When the bridge is started, it will download the config from the given URL and
save it to the file specified with the `-c` flag (defaults to `config.yaml`).

There should also be some "logout" button that forgets the URL and deletes the
config file.

## IPC
The protocol is based on sending JSON objects separated by newlines (`\n`).

Requests can be sent in both directions. Requests must contain a `command`
field that specifies the type of request.

Requests can also contain an `id` field  with an integer value, which is used
when responding to the request. If the `id` field is not present, a response
must not be sent. If the `id` field is present, a response must be sent, even
if the command is not recognized. Responses must use a type of `response` or
`error` with the same ID as the request.

IDs should never be reused within the same connection. An incrementing integer
is a good option for unique request IDs.

All other request parameters and response data must be in the `data` field.
The field may be an array or an object depending on the request type.

Responses with `"command": "error"` must include an object in the `data` field
with a human-readable error message in the `message` field and some simple
error code in the `code` field.

### Examples

```json
{
  "command": "get_chat",
  "id": 123,
  "data": {
    "chat_guid": "iMessage;+;chat123456"
  }
}
```

Success response:

```json
{
  "command": "response",
  "id": 123,
  "data": {
    "title": "iMessage testing",
    "members": ["+1234567890", "+3581234567", "user@example.com"]
  }
}
```

Error response:

```json
{
  "command": "error",
  "id": 123,
  "data": {
    "code": "not_found",
    "message": "That chat does not exist"
  }
}
```

Another error response:

```json
{
  "command": "error",
  "id": 123,
  "data": {
    "code": "unknown_command",
    "message": "Unknown command 'get_chat'"
  }
}
```

### Requests

#### to Brooklyn
* Send a message (request type `send_message`)
  * `chat_guid` (str) - Chat identifier
  * `text` (str) - Text to send
  * Response should contain the sent message `guid`, `timestamp`, and (preliminary) `service`
    * If the service is omitted, it defaults to the service of the chat.
* Send a media message (request type `send_media`)
  * `chat_guid` (str) - Chat identifier
  * `text` (str) - An optional caption to send with the media
  * `path_on_disk` (str) - The path to the file on disk
  * `file_name` (str) - The user-facing name of the file
  * `mime_type` (str) - The mime type of the file
  * Response should contain the sent message `guid` and `timestamp`
* Send (or remove) a tapback (request type `send_tapback`)
  * `chat_guid` (str) - Chat identifier
  * `target_guid` (str) - The target message ID
  * `target_part` (int) - The target message part index
  * `type` (int) - The type of tapback to send
  * `metadata` (any) - Metadata to send with the message. Pass any valid JSON
  * Response should contain the sent tapback `guid` and `timestamp`
  * Removing tapbacks is done by sending a 300x type instead of 200x (same as iMessage internally)
* Send a read receipt (request type `send_read_receipt`)
  * `chat_guid` (str) - Chat identifier
  * `read_up_to` (str, UUID) - The GUID of the last read message
* Send a typing notification (request type `set_typing`)
  * `chat_guid` (str) - The chat where the user is typing.
  * `typing` (bool) - Whether to send or cancel the typing notification.
* Get list of chats with messages after date (request type `get_chats`)
  * `min_timestamp` (double) - Unix timestamp
  * Response should be an array of chat GUIDs
* Get chat info (request type `get_chat`)
  * `chat_guid` (str) - Chat identifier, e.g. `iMessage;+;chat123456`
  * Response contains:
    * `title` (displayname of group, if it is one)
    * `members` (list of participant user identifiers)
    * `correlation_id` (the UUID for this conversation, if one exists)
* Get group chat avatar (request type `get_chat_avatar`)
  * `chat_guid` (str) - Group chat identifier
  * Response contains the same data as message `attachment`s: `mime_type`,
    `path_on_disk` and `file_name`
* Get contact info (request type `get_contact`)
  * `user_guid` (str) - User identifier, e.g. `iMessage;-;+123456`
    or `SMS;-;+123456`
  * Returns contact info
    * `first_name` (str)
    * `last_name` (str)
    * `nickname` (str)
    * `avatar` (base64 str) - The avatar image data. I think they're small
      enough that it doesn't need to go through the disk.
    * `phones` (list of str)
    * `emails` (list of str)
    * `correlation_id` (str, optional) - The UUID identifying this person
* Get full contact list (request type `get_contact_list`)
  * Returns an object with a `contacts` key that contains a list of contacts in the same format as `get_contact`
  * There should be an additional `primary_identifier` field if the primary identifier of the contact is known.
    * When the user starts a chat with the contact, that identifier will be passed to `resolve_identifier`.
  * Avatars can be omitted in this case.
* Get messages after a specific timestamp (request type `get_messages_after`)
  * Request includes `chat_guid` and `timestamp`
  * Returns list of messages (see incoming messages format below)
  * List should be sorted by timestamp in ascending order
* Get X most recent messages (request type `get_recent_messages`)
  * Request includes `chat_guid` and `limit`
  * Same return type as with `get_messages_after`
* Resolve an identifier into a private chat GUID (request type `resolve_identifier`)
  * `identifier` (str) - International phone number or email
  * Returns `guid` (str) with a GUID for the chat with the user.
  * If the identifier isn't valid or messages can't be sent to it, return a
    standard error response with an appropriate message.
* Prepare for startup sync (request type `pre_startup_sync`)
  * Sent when the bridge is starting and is about to do the startup sync.
    The sync won't start until this request responds.
  * Optionally, the response may contain `"skip_sync": true` to skip the startup sync.
* Prepare a new private chat (request type `prepare_dm`)
  * `guid` (str) - The GUID of the user to start a chat with
  * Doesn't return anything (just acknowledge with an empty response).
* Confirm a message being bridged (request type `message_bridge_result`).
  * Has fields `chat_guid`, `message_guid` and `success`.
  * Doesn't have an ID, so it doesn't need to be responded to.
  * Only enabled for android-sms.
* Notification of portal mxID for a chat GUID (request type `chat_bridge_result`)
  * Has fields `chat_guid`, `mxid`
  * Only enabled for android-sms.

#### to mautrix-imessage
* Incoming messages (request type `message`)
  * `guid` (str, UUID) - Global message ID
  * `timestamp` (double) - Unix timestamp
  * `subject` (str) - Message subject, usually empty
  * `text` (str) - Message text
  * `chat_guid` (str) - Chat identifier, e.g. `iMessage;+;chat<number>`,
    `iMessage;-;+123456` or `SMS;-;+123456`
  * `sender_guid` (str) - User identifier, e.g. `iMessage;-;+123456` or
    `SMS;-;+123456`. Not required if `is_from_me` is true.
  * `is_from_me` (bool) - True if the message was sent by the local user
  * `service` (str) - Explicitly states the origin service (e.g. `SMS`, `iMessage`), most useful when using chat merging
    * If the service is omitted, it defaults to the service of the chat.
  * `thread_originator_guid` (str, UUID, optional) - The thread originator message ID
  * `thread_originator_part` (int) - The thread originator message part index (e.g. 0)
  * `attachments` (list of objects, optional) - Attachment info (media messages, maybe stickers?)
    * `mime_type` (str, optional) - The mime type of the file, optional
    * `file_name` (str) - The user-facing file name
    * `path_on_disk` (str) - The file path on disk that the bridge can read
  * `associated_message` (object, optional) - Associated message info (tapback/sticker)
    * `target_guid` (str) - The message that this event is targeting, e.g. `p:0/<uuid>`
    * `type` (int) - The type of association (1000 = sticker, 200x = tapback, 300x = tapback remove)
  * `error_notice` (str, optional) - An error notice to send to Matrix. Can be a dedicated message (with `item_type` = -100) or a part of a real message.
  * `item_type` (int, optional) - Message type, 0 = normal message, 1 = member change, 2 = name change, 3 = avatar change, -100 = error notice.
  * `group_action_type` (int, optional) - Group action type, which is a subtype of `item_type`
    * For member changes, 0 = add member, 1 = remove member
    * For avatar changes, 1 = set avatar, 2 = remove avatar
  * `target_guid` (str, optional) - For member change messages, the user identifier of the user being changed.
  * `new_group_title` (str, optional) - New name for group when the message was a group name change
  * `metadata` (any) - Metadata sent with the message. Any valid JSON may be present here.
  * `correlation_id` (str, optional) - The UUID identifying the conversation this message is a part of
  * `sender_correlation_id` (str, optional) - The UUID identifying the person who sent this message
* Incoming read receipts (request type `read_receipt`)
  * `sender_guid` (str) - the user who sent the read receipt. Not required if `is_from_me` is true.
  * `is_from_me` (bool) - True if the read receipt is from the local user (e.g. from another device).
  * `chat_guid` (str) - The chat where the read receipt is.
  * `read_up_to` (str, UUID) - The GUID of the last read message.
  * `read_at` (double) - Unix timestamp when the read receipt happened.
  * `correlation_id` (str, optional) - The UUID identifying the conversation this receipt is a part of
  * `sender_correlation_id` (str, optional) - The UUID identifying the person who sent this receipt
* Incoming typing notifications (request type `typing`)
  * `chat_guid` (str) - The chat where the user is typing.
  * `correlation_id` (str, optional) - The UUID identifying the conversation this event is a part of
  * `typing` (bool) - Whether the user is typing or not.
* Chat info changes and new chats (request type `chat`)
  * Same info as `get_chat` responses: `title`, `correlation_id`, `members`, plus a `chat_guid` field to identify the chat.
  * `no_create_room` can be set to `true` to disable creating a new room if one doesn't exist.
* Chat ID change (request type `chat_id`)
  * `old_guid` (str) - The old chat GUID.
  * `new_guid` (str) - The new chat GUID.
  * Returns `changed` with a boolean indicating whether the change was applied.
    * If false, it means a chat with the new GUID already existed, or a chat with the old GUID didn't exist.
* Contact info changes (request type `contact`)
  * Same info as `get_contact` responses, plus a `user_guid` field to identify the contact.
* Outgoing message status (request type `send_message_status`)
  * `guid` (str, UUID) - The GUID of the message that the status update is about.
  * `chat_guid` (str) - The GUID of the chat from which this message originated
  * `status` (str, enum) - The current status of the message.
    * Allowed values: `sent`, `delivered`, `failed`
  * `message` (str) - A human-readable description of the status, if needed.
  * `status_code` (str) - A machine-readable identifier for the current status.
  * `service` (str) - The service the outgoing message will be sent on. If the message is downgraded to SMS, you should send this payload again with service set to `SMS`.
    * If the service is omitted, it defaults to the service of the chat.
  * `correlation_id` (str, optional) - The UUID identifying the conversation this receipt is a part of
  * `sender_correlation_id` (str, optional) - The UUID identifying the person who sent the message this receipt is a part of
* Pinging the Matrix websocket (request type `ping_server`)
  * Used to ensure that the websocket connection is alive. Should be called if there's some reason to believe
    the connection may have silently failed, e.g. when the device wakes up from sleep.
  * Doesn't take any parameters. Responds with three timestamps: `start`, `server` and `end`.
* Sending status updates (request type `bridge_status`)
  * Inform the server about iMessage connection issues.
  * `state_event` (str, enum) - The state of the bridge.
    * Allowed values: `STARTING`, `UNCONFIGURED`, `CONNECTING`, `BACKFILLING`, `CONNECTED`, `TRANSIENT_DISCONNECT`, `BAD_CREDENTIALS`, `UNKNOWN_ERROR`, `LOGGED_OUT`
  * `error` (str) - An error code that the user's client application can use if it needs to do something special to handle the error.
  * `message` (str) - Human-readable error message.
  * `remote_id` (str, optional) - The iMessage user ID of the bridge user.
  * `remote_name` (str, optional) - The iMessage displayname of the bridge user.
* Get bridged message IDs after certain time (request type `message_ids_after_time`)
  * `chat_guid` (str) - The chat GUID to get the message IDs from.
  * `after_time` (double) - The unix timestamp after which to find messages.
