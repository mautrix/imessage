# Features & roadmap
✔️ = feature is supported  
❌ = feature is not yet supported  
🛑 = feature is not possible

Note that Barcelona, which the mac-nosip connector uses, is no longer maintained.

## Matrix → iMessage
| Feature              | mac | mac-nosip | bluebubbles |
|----------------------|-----|-----------|-------------|
| Plain text           | ✔️  | ✔️        | ✔️          |
| Media/files          | ✔️  | ✔️        | ✔️          |
| Replies              | 🛑  | ✔️        | ✔️†         |
| Reactions            | 🛑  | ✔️        | ✔️          |
| Edits                | 🛑  | ❌         | ✔️*          |
| Unsends              | 🛑  | ❌         | ✔️*           |
| Redactions           | 🛑  | ✔️        | ✔️          |
| Read receipts        | 🛑  | ✔️        | ✔️          |
| Typing notifications | 🛑  | ✔️        | ✔️          |

† BlueBubbles had bugs with replies until v1.9.5

\* macOS Ventura or higher is required

## iMessage → Matrix
| Feature                          | mac | mac-nosip | bluebubbles |
|----------------------------------|-----|-----------|-------------|
| Plain text                       | ✔️  | ✔️        | ✔️          |
| Media/files                      | ✔️  | ✔️        | ✔️          |
| Replies                          | ✔️  | ✔️        | ✔️          |
| Tapbacks                         | ✔️  | ✔️        | ✔️          |
| Edits                            | ❌   | ❌         | ✔️ (BlueBubbles Server >= 1.9.6)           |
| Unsends                          | ❌   | ❌         | ✔️ (BlueBubbles Server >= 1.9.6)           |
| Own read receipts                | ✔️  | ✔️        | ✔️          |
| Other read receipts              | ✔️  | ✔️        | ✔️          |
| Typing notifications             | 🛑  | ✔️        | ✔️          |
| User metadata                    | ✔️  | ✔️        | ✔️          |
| Group metadata                   | ✔️  | ✔️        | ✔️          |
| Group Participants Added/Removed | ✔️  | ✔️        | ✔️          |
| Backfilling history              | ✔️‡  | ✔️‡        | ✔️‡         |

‡ Backfilling tapbacks is not yet supported

## Android SMS
The android-sms connector is deprecated in favor of [mautrix-gmessages](https://github.com/mautrix/gmessages).

#### Supported
* Plain text (SMS)
* Media (MMS)
* Group chats
* Backfilling history from the Android SMS database.
* Storing messages in the Android SMS database
  (so you can still switch to a different SMS app).

#### Not supported
* RCS (there's no API for it, it's exclusive to Google's Messages app).
* Any features that SMS/MMS don't support
  (replies, reactions, read receipts, typing notifications).

## Misc
* [x] Automatic portal creation
  * [x] At startup
  * [x] When receiving message
* [ ] Private chat creation by inviting Matrix puppet of iMessage user to new room
* [x] Option to use own Matrix account for messages sent from other iMessage clients
  * [x] Automatically with shared secret login
  * [x] Manually with `login-matrix` command
