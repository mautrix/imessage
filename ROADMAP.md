# Features & roadmap
✔️ = feature is supported  
❌ = feature is planned, but not yet supported  
🛑 = feature is not possible

## Matrix → iMessage
| Feature              | macOS | iOS | macOS (no SIP) |
|----------------------|-------|-----|----------------|
| Plain text           | ✔️    | ✔️  | ✔️             |
| Media/files          | ✔️    | ✔️  | ✔️             |
| Replies              | 🛑    | ?   | ✔️             |
| Reactions            | 🛑    | ?   | ✔️             |
| Read receipts        | 🛑    | ?   | ✔️             |
| Typing notifications | 🛑    | ?   | ✔️             |

## iMessage → Matrix
| Feature              | macOS | iOS | macOS (no SIP) |
|----------------------|-------|-----|----------------|
| Plain text           | ✔️    | ✔️  | ✔️             |
| Media/files          | ✔️    | ✔️  | ✔️             |
| Replies              | ✔️    | ❌  | ✔️             |
| Tapbacks             | ✔️    | ❌  | ✔️             |
| Own read receipts    | ✔️    | ❌  | ✔️             |
| Other read receipts  | ✔️    | ❌  | ✔️             |
| Typing notifications | 🛑    | ✔️  | ✔️             |
| User metadata        | ✔️    | ✔️  | ✔️             |
| Group metadata       | ✔️    | ✔️  | ✔️             |
| Backfilling history  | ✔️    | ✔️  | ✔️             |

## Android SMS
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
* [ ] Option to use own Matrix account for messages sent from other iMessage clients
  * [x] Automatically with shared secret login
  * [ ] Manually with `login-matrix` command
