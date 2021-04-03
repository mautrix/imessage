# iMessage bridge protocol

## Setup
The bridge needs a config file that has the homeserver details, access tokens
and other such things. Brooklyn needs to get that config file from somewhere
and point the bridge at it when running. The setup UX should just be scanning
a QR code.

1. User scans QR code with Brooklyn on iPhone. The QR code contains a URL,
   which may end in a newline. Strip away the newline if necessary.
2. Make a HEAD (or GET) request to the URL in the QR code.
	* If the response is a redirect, store the target URL.
	* If not, store the URL in the QR code.
2. Download the config from the stored URL.
3. Start the mautrix-imessage subprocess, pointing it to the downloaded config
   file using the `-c` flag.

Whenever the app is updated, the config should be refetched from the stored URL.
There should also be some "logout" button that forgets the URL and deletes the
config file.

## IPC
The protocol is based on sending JSON objects separated by newlines (`\n`).

Requests can be sent in both directions. Requests must contain a `command` field
that specifies the type of request.

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

### Requests

#### to Brooklyn
* Send a message (request type `send_message`)
	* `chat_guid` (str) - Chat identifier
	* `text` (str) - Text to send
	* Response should contain the sent message `guid` and `timestamp`
* Send a media message (request type `send_media`)
	* `chat_guid` (str) - Chat identifier
	* `path_on_disk` (str) - The path to the file on disk
	* `file_name` (str) - The user-facing name of the file
	* `mime_type` (str) - The mime type of the file
	* Response should contain the sent message `guid` and `timestamp`
* Send (or remove) a tapback (request type `send_tapback`)
	* `chat_guid` (str) - Chat identifier
	* `target_guid` (str) - The target message ID
	* `type` (int) - The type of tapback to send
	* Response should contain the sent tapback `guid` and `timestamp`
* Send a read receipt (request type `send_read_receipt`)
	* `chat_guid` (str) - Chat identifier
	* `read_up_to` (str, UUID) - The GUID of the last read message
* Get list of chats with messages after date (request type `get_chats`)
	* `min_timestamp` (double) - Unix timestamp
	* Response should be an array of chat GUIDs
* Get group chat info (request type `get_chat`)
	* `chat_guid` (str) - Group chat identifier, e.g. `iMessage;+;chat123456`
	* Response contains `title` (displayname of group) and `members` (list of participant user identifiers)
* Get group chat avatar (request type `get_chat_avatar`)
	* `chat_guid` (str) - Group chat identifier
	* Response contains the same data as message `attachment`s: `mime_type`, `path_on_disk` and `file_name`
* Get contact info (request type `get_contact`)
	* `user_guid` (str) - User identifier, e.g. `iMessage;-;+123456` or `SMS;-;+123456`
	* Returns contact info
		* `first_name` (str)
		* `last_name` (str)
		* `nickname` (str)
		* `avatar` (base64 str) - The avatar image data. I think they're small enough that it doesn't need to go through the disk.
		* `phones` (list of str)
		* `emails` (list of str)
* Get messages after a specific timestamp (request type `get_messages_after`)
	* Request includes `chat_guid` and `timestamp`
	* Returns list of messages (see incoming messages format below)
	* List should be sorted by timestamp in ascending order
* Get X most recent messages (request type `get_recent_messages`)
	* Request includes `chat_guid` and `limit`
	* Same return type as with `get_messages_after`

#### to mautrix-imessage
* Incoming messages (request type `message`)
	* `guid` (str, UUID) - Global message ID
	* `timestamp` (double) - Unix timestamp
	* `subject` (str) - Message subject, usually empty
	* `text` (str) - Message text
	* `chat_guid` (str) - Chat identifier, e.g. `iMessage;+;chat<number>`, `iMessage;-;+123456` or `SMS;-;+123456`
	* `sender_guid` (str) - User identifier, e.g. `iMessage;-;+123456` or `SMS;-;+123456`. Not required if `is_from_me` is true.
	* `is_from_me` (bool) - True if the message was sent by the local user
	* `thread_originator_guid` (str, UUID, optional) - The thread originator message ID
	* `attachment` (object, optional) - Attachment info (media messages, maybe stickers?)
		* `mime_type` (str, optional) - The mime type of the file, optional
		* `file_name` (str) - The user-facing file name
		* `path_on_disk` (str) - The file path on disk that the bridge can read
	* `associated_message` (object, optional) - Associated message info (tapback/sticker)
		* `target_guid` (str) - The message that this event is targeting, e.g. `p:0/<uuid>`
		* `type` (int) - The type of association (1000 = sticker, 200x = tapback, 300x = tapback remove)
	* `group_action_type` (int, optional) - Group action type, 1 = set avatar, 2 = remove avatar
	* `new_group_title` (str, optional) - New name for group when the message was a group name change
* Incoming read receipts (request type `read_receipt`)
	* `sender_guid` (str) - the user who sent the read receipt. Not required if `is_from_me` is true.
	* `is_from_me` (bool) - True if the read receipt is from the local user (e.g. from another device)
	* `chat_guid` (str) - The chat where the read receipt is
	* `read_up_to` (str, UUID) - The GUID of the last read message
