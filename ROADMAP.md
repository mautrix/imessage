# Features & roadmap
✔️ = feature is supported  
❌ = feature is planned, but not yet supported  
🛑 = feature is not possible

## Matrix → iMessage
| Feature              | macOS | iOS | macOS (no SIP) |
|----------------------|-------|-----|----------------|
| Plain text           | ✔️    | ✔️  | ✔️             |
| Media/files          | ✔️    | ✔️  | ✔️             |
| Replies              | 🛑    | ❌  | ✔️             |
| Reactions            | 🛑    | ❌  | ✔️             |
| Read receipts        | 🛑    | ❌  | ❌             |
| Typing notifications | 🛑    | ❌  | ❌             |

## iMessage → Matrix
| Feature              | macOS | iOS |
|----------------------|-------|-----|
| Plain text           | ✔️    | ✔️  |
| Media/files          | ✔️    | ✔️  |
| Replies              | ✔️    | ❌  |
| Tapbacks             | ✔️    | ❌  |
| Own read receipts    | ✔️    | ❌  |
| Other read receipts  | ✔️    | ❌  |
| Typing notifications | 🛑    | ✔️  |
| User metadata        | ✔️    | ✔️  |
| Group metadata       | ✔️    | ✔️  |
| Backfilling history  | ✔️    | ✔️  |

## Misc
* [x] Automatic portal creation
  * [x] At startup
  * [x] When receiving message
* [ ] Private chat creation by inviting Matrix puppet of iMessage user to new room
* [ ] Option to use own Matrix account for messages sent from other iMessage clients
  * [x] Automatically with shared secret login
  * [ ] Manually with `login-matrix` command
