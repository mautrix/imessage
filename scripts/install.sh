#!/bin/bash
set -euo pipefail

BINARY="$1"
DATA_DIR="$2"
BUNDLE_ID="$3"

BINARY="$(cd "$(dirname "$BINARY")" && pwd)/$(basename "$BINARY")"
CONFIG="$DATA_DIR/config.yaml"
REGISTRATION="$DATA_DIR/registration.yaml"
PLIST="$HOME/Library/LaunchAgents/$BUNDLE_ID.plist"

echo ""
echo "═══════════════════════════════════════════════"
echo "  iMessage Bridge Setup"
echo "═══════════════════════════════════════════════"
echo ""

# ── Prompt for config values ──────────────────────────────────
FIRST_RUN=false
if [ -f "$CONFIG" ]; then
    echo "Config already exists at $CONFIG"
    echo "Skipping configuration prompts. Delete it to re-configure."
    echo ""
else
    FIRST_RUN=true

    read -p "Homeserver URL [http://localhost:8008]: " HS_ADDRESS
    HS_ADDRESS="${HS_ADDRESS:-http://localhost:8008}"

    read -p "Homeserver domain (the server_name, e.g. example.com): " HS_DOMAIN
    if [ -z "$HS_DOMAIN" ]; then
        echo "ERROR: Domain is required." >&2
        exit 1
    fi

    read -p "Your Matrix ID [@you:$HS_DOMAIN]: " ADMIN_USER
    ADMIN_USER="${ADMIN_USER:-@you:$HS_DOMAIN}"

    echo ""
    echo "Database:"
    echo "  1) PostgreSQL (recommended)"
    echo "  2) SQLite"
    read -p "Choice [1]: " DB_CHOICE
    DB_CHOICE="${DB_CHOICE:-1}"

    if [ "$DB_CHOICE" = "1" ]; then
        DB_TYPE="postgres"
        read -p "PostgreSQL URI [postgres://localhost/mautrix_imessage?sslmode=disable]: " DB_URI
        DB_URI="${DB_URI:-postgres://localhost/mautrix_imessage?sslmode=disable}"
    else
        DB_TYPE="sqlite3-fk-wal"
        DB_URI="file:$DATA_DIR/mautrix-imessage.db?_txlock=immediate"
    fi

    echo ""

    # ── Generate config ───────────────────────────────────────────
    mkdir -p "$DATA_DIR"
    "$BINARY" -c "$CONFIG" -e 2>/dev/null
    echo "✓ Generated config"

    # Patch values into the generated config
    python3 -c "
import re, sys
text = open('$CONFIG').read()

def patch(text, key, val):
    return re.sub(
        r'^(\s+' + re.escape(key) + r'\s*:)\s*.*$',
        r'\1 ' + val,
        text, count=1, flags=re.MULTILINE
    )

text = patch(text, 'address', '$HS_ADDRESS')
text = patch(text, 'domain', '$HS_DOMAIN')
text = patch(text, 'type', '$DB_TYPE')
text = patch(text, 'uri', '$DB_URI')

lines = text.split('\n')
in_perms = False
for i, line in enumerate(lines):
    if 'permissions:' in line and not line.strip().startswith('#'):
        in_perms = True
        continue
    if in_perms and line.strip() and not line.strip().startswith('#'):
        indent = len(line) - len(line.lstrip())
        lines[i] = ' ' * indent + '\"$ADMIN_USER\": admin'
        break
text = '\n'.join(lines)

open('$CONFIG', 'w').write(text)
"
    # iMessage CloudKit chats can have tens of thousands of messages.
    # Deliver all history in one forward batch to avoid DAG fragmentation.
    sed -i '' 's/max_initial_messages: [0-9]*/max_initial_messages: 2147483647/' "$CONFIG"
    sed -i '' 's/max_catchup_messages: [0-9]*/max_catchup_messages: 5000/' "$CONFIG"
    sed -i '' 's/batch_size: [0-9]*/batch_size: 10000/' "$CONFIG"
    sed -i '' 's/max_batches: 0$/max_batches: -1/' "$CONFIG"
    # Use 1s between batches — fast enough for backfill, prevents idle hot-loop
    sed -i '' 's/batch_delay: [0-9]*/batch_delay: 1/' "$CONFIG"
    echo "✓ Configured: $HS_ADDRESS, $HS_DOMAIN, $ADMIN_USER, $DB_TYPE"
fi

# ── Ensure backfill_source key exists in config ───────────────
if ! grep -q 'backfill_source:' "$CONFIG" 2>/dev/null; then
    sed -i '' '/cloudkit_backfill:/a\
    backfill_source: cloudkit' "$CONFIG"
fi

# ── Backfill source selection ─────────────────────────────────
# On first run (fresh DB), show a 3-way prompt. On re-runs, preserve existing.
DB_PATH_CHECK=$(python3 -c "
import re
text = open('$CONFIG').read()
m = re.search(r'^\s+uri:\s*file:([^?]+)', text, re.MULTILINE)
print(m.group(1) if m else '')
")
IS_FRESH_DB=false
if [ -z "$DB_PATH_CHECK" ] || [ ! -f "$DB_PATH_CHECK" ]; then
    IS_FRESH_DB=true
fi

CURRENT_BACKFILL=$(grep 'cloudkit_backfill:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*cloudkit_backfill: *//' || true)
CURRENT_SOURCE=$(grep 'backfill_source:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*backfill_source: *//' || true)

if [ -t 0 ]; then
    if [ "$IS_FRESH_DB" = "true" ]; then
        echo ""
        echo "Message History Backfill:"
        echo "  1) iCloud (CloudKit) — sync from iCloud, requires device PIN"
        echo "  2) Local chat.db — for legacy systems, read macOS Messages database, requires Full Disk Access"
        echo "  3) Disabled — real-time messages only"
        echo ""
        read -p "Choose [1/2/3]: " BACKFILL_CHOICE
        case "$BACKFILL_CHOICE" in
            2)
                sed -i '' "s/cloudkit_backfill: .*/cloudkit_backfill: true/" "$CONFIG"
                sed -i '' "s/backfill_source: .*/backfill_source: chatdb/" "$CONFIG"
                echo "✓ Chat.db backfill enabled — requires Full Disk Access for the bridge binary"
                ;;
            3)
                sed -i '' "s/cloudkit_backfill: .*/cloudkit_backfill: false/" "$CONFIG"
                sed -i '' "s/backfill_source: .*/backfill_source: cloudkit/" "$CONFIG"
                echo "✓ Backfill disabled — real-time messages only, no PIN needed"
                ;;
            *)
                sed -i '' "s/cloudkit_backfill: .*/cloudkit_backfill: true/" "$CONFIG"
                sed -i '' "s/backfill_source: .*/backfill_source: cloudkit/" "$CONFIG"
                echo "✓ CloudKit backfill enabled — you'll be asked for your device PIN during login"
                echo ""
                echo "IMPORTANT: Before starting the bridge, sync your latest messages to iCloud"
                echo "from an Apple device (iPhone, iPad, or Mac) to ensure all recent messages"
                echo "are available for backfill."
                echo ""
                read -p "Have you synced your Apple device to iCloud? [y/N]: " ICLOUD_SYNCED
                case "$ICLOUD_SYNCED" in
                    [yY]*) echo "✓ Great — backfill will include your latest messages" ;;
                    *)     echo "⚠ Please sync your Apple device to iCloud before starting the bridge" ;;
                esac
                ;;
        esac
    else
        # Re-run: show current setting
        if [ "$CURRENT_BACKFILL" = "true" ] && [ "$CURRENT_SOURCE" = "chatdb" ]; then
            echo "✓ Backfill source: chat.db (local macOS Messages database)"
        elif [ "$CURRENT_BACKFILL" = "true" ]; then
            echo "✓ Backfill source: CloudKit (iCloud sync)"
        else
            echo "✓ Backfill: disabled (real-time messages only)"
        fi
    fi
fi

# ── Full Disk Access check for chat.db mode (macOS only) ──────
CURRENT_SOURCE=$(grep 'backfill_source:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*backfill_source: *//' || true)
if [ "$CURRENT_SOURCE" = "chatdb" ] && [ "$(uname -s)" = "Darwin" ]; then
    CHATDB_PATH="$HOME/Library/Messages/chat.db"
    if [ -f "$CHATDB_PATH" ]; then
        if ! sqlite3 "$CHATDB_PATH" "SELECT 1 FROM message LIMIT 1" >/dev/null 2>&1; then
            echo ""
            echo "⚠ Full Disk Access is required for chat.db backfill."
            echo "  Opening System Settings → Privacy & Security → Full Disk Access..."
            echo "  Grant access to the bridge binary, then press Enter to continue."
            open "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles" 2>/dev/null
            read -p "Press Enter when Full Disk Access has been granted..."
            if sqlite3 "$CHATDB_PATH" "SELECT 1 FROM message LIMIT 1" >/dev/null 2>&1; then
                echo "✓ Full Disk Access confirmed"
            else
                echo "⚠ chat.db still not accessible — the bridge will prompt again on startup"
            fi
        else
            echo "✓ Full Disk Access: granted"
        fi
    else
        echo "⚠ chat.db not found at $CHATDB_PATH — is Messages set up on this Mac?"
    fi
fi

# ── Max initial messages (new database + CloudKit backfill + interactive) ──
CURRENT_BACKFILL=$(grep 'cloudkit_backfill:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*cloudkit_backfill: *//' || true)
if [ "$CURRENT_BACKFILL" = "true" ] && [ -t 0 ]; then
    DB_PATH=$(python3 -c "
import re
text = open('$CONFIG').read()
m = re.search(r'^\s+uri:\s*file:([^?]+)', text, re.MULTILINE)
print(m.group(1) if m else '')
")
    if [ -z "$DB_PATH" ] || [ ! -f "$DB_PATH" ]; then
        echo ""
        echo "By default, all messages per chat will be backfilled."
        echo "If you choose to limit, the minimum is 100 messages per chat."
        read -p "Would you like to limit the number of messages? [y/N]: " LIMIT_MSGS
        case "$LIMIT_MSGS" in
            [yY]*)
                while true; do
                    read -p "Max messages per chat (minimum 100): " MAX_MSGS
                    MAX_MSGS=$(echo "$MAX_MSGS" | tr -dc '0-9')
                    if [ -n "$MAX_MSGS" ] && [ "$MAX_MSGS" -ge 100 ] 2>/dev/null; then
                        break
                    fi
                    echo "Minimum is 100. Please enter a value of 100 or more."
                done
                sed -i '' "s/max_initial_messages: [0-9]*/max_initial_messages: $MAX_MSGS/" "$CONFIG"
                # Disable backward backfill so the cap is the final word on message count
                sed -i '' 's/max_batches: -1$/max_batches: 0/' "$CONFIG"
                echo "✓ Max initial messages set to $MAX_MSGS per chat"
                ;;
            *)
                echo "✓ Backfilling all messages"
                ;;
        esac
    fi
fi

# Tune backfill settings when CloudKit backfill is enabled
CURRENT_BACKFILL=$(grep 'cloudkit_backfill:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*cloudkit_backfill: *//' || true)
if [ "$CURRENT_BACKFILL" = "true" ]; then
    # iMessage CloudKit chats can have tens of thousands of messages.
    # Deliver all history in one forward batch to avoid DAG fragmentation.
    PATCHED_BACKFILL=false
    # Only enable unlimited backward backfill when max_initial is uncapped.
    # When the user caps max_initial_messages, max_batches stays at 0 so the
    # bridge won't backfill beyond the cap.
    if grep -q 'max_initial_messages: 2147483647' "$CONFIG" 2>/dev/null; then
        if grep -q 'max_batches: 0$' "$CONFIG" 2>/dev/null; then
            sed -i '' 's/max_batches: 0$/max_batches: -1/' "$CONFIG"
            PATCHED_BACKFILL=true
        fi
    fi
    if grep -q 'max_initial_messages: [0-9]\{1,2\}$' "$CONFIG" 2>/dev/null; then
        sed -i '' 's/max_initial_messages: [0-9]*/max_initial_messages: 2147483647/' "$CONFIG"
        PATCHED_BACKFILL=true
    fi
    if grep -q 'max_catchup_messages: [0-9]\{1,3\}$' "$CONFIG" 2>/dev/null; then
        sed -i '' 's/max_catchup_messages: [0-9]*/max_catchup_messages: 5000/' "$CONFIG"
        PATCHED_BACKFILL=true
    fi
    if grep -q 'batch_size: [0-9]\{1,3\}$' "$CONFIG" 2>/dev/null; then
        sed -i '' 's/batch_size: [0-9]*/batch_size: 10000/' "$CONFIG"
        PATCHED_BACKFILL=true
    fi
    if grep -q 'batch_delay: 0$' "$CONFIG" 2>/dev/null; then
        sed -i '' 's/batch_delay: 0$/batch_delay: 1/' "$CONFIG"
        PATCHED_BACKFILL=true
    fi
    if [ "$PATCHED_BACKFILL" = true ]; then
        echo "✓ Updated backfill settings (max_initial=unlimited, batch_size=10000, max_batches=-1)"
    fi
fi

# ── Read domain from config (works on first run and re-runs) ──
HS_DOMAIN=$(python3 -c "
import re
text = open('$CONFIG').read()
m = re.search(r'^\s+domain:\s*(\S+)', text, re.MULTILINE)
print(m.group(1) if m else 'yourserver')
")

# ── Generate registration ────────────────────────────────────
if [ -f "$REGISTRATION" ]; then
    echo "✓ Registration already exists"
else
    "$BINARY" -c "$CONFIG" -g -r "$REGISTRATION" 2>/dev/null
    echo "✓ Generated registration"
fi

# ── Register with homeserver (first run only) ─────────────────
if [ "$FIRST_RUN" = true ]; then
    REG_PATH="$(cd "$DATA_DIR" && pwd)/registration.yaml"
    echo ""
    echo "┌─────────────────────────────────────────────┐"
    echo "│  Register with your homeserver:             │"
    echo "│                                             │"
    echo "│  Add to homeserver.yaml:                    │"
    echo "│    app_service_config_files:                │"
    echo "│      - $REG_PATH"
    echo "│                                             │"
    echo "│  Then restart your homeserver.              │"
    echo "└─────────────────────────────────────────────┘"
    echo ""
    read -p "Press Enter once your homeserver is restarted..."
fi

# ── Restore CardDAV config from backup ────────────────────────
# Skip when using chat.db — local macOS Contacts are used automatically.
CARDDAV_BACKUP="$DATA_DIR/.carddav-config"
CURRENT_SOURCE_CHECK=$(grep 'backfill_source:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*backfill_source: *//' || true)
if [ "$CURRENT_SOURCE_CHECK" != "chatdb" ] && [ -f "$CARDDAV_BACKUP" ]; then
    CHECK_EMAIL=$(grep 'email:' "$CONFIG" 2>/dev/null | head -1 | sed "s/.*email: *//;s/['\"]//g" | tr -d ' ' || true)
    if [ -z "$CHECK_EMAIL" ]; then
        source "$CARDDAV_BACKUP"
        if [ -n "${SAVED_CARDDAV_EMAIL:-}" ] && [ -n "${SAVED_CARDDAV_ENC:-}" ]; then
            python3 -c "
import re
text = open('$CONFIG').read()
def patch(text, key, val):
    return re.sub(r'^(\s+' + re.escape(key) + r'\s*:)\s*.*$', r'\1 ' + val, text, count=1, flags=re.MULTILINE)
text = patch(text, 'email', '\"$SAVED_CARDDAV_EMAIL\"')
text = patch(text, 'url', '\"$SAVED_CARDDAV_URL\"')
text = patch(text, 'username', '\"$SAVED_CARDDAV_USERNAME\"')
text = patch(text, 'password_encrypted', '\"$SAVED_CARDDAV_ENC\"')
open('$CONFIG', 'w').write(text)
"
            echo "✓ Restored CardDAV config: $SAVED_CARDDAV_EMAIL"
        fi
    fi
fi

# ── Contact source (runs every time, can reconfigure) ─────────
# Skip when using chat.db — local macOS Contacts are used automatically.
CURRENT_SOURCE=$(grep 'backfill_source:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*backfill_source: *//' || true)
if [ "$CURRENT_SOURCE" = "chatdb" ]; then
    echo "✓ Contact source: local macOS Contacts (via chat.db)"
elif [ -t 0 ]; then
    CURRENT_CARDDAV_EMAIL=$(grep 'email:' "$CONFIG" 2>/dev/null | head -1 | sed "s/.*email: *//;s/['\"]//g" | tr -d ' ' || true)
    CONFIGURE_CARDDAV=false

    if [ -n "$CURRENT_CARDDAV_EMAIL" ] && [ "$CURRENT_CARDDAV_EMAIL" != '""' ]; then
        echo ""
        echo "Contact source: External CardDAV ($CURRENT_CARDDAV_EMAIL)"
        read -p "Change contact provider? [y/N]: " CHANGE_CONTACTS
        case "$CHANGE_CONTACTS" in
            [yY]*) CONFIGURE_CARDDAV=true ;;
        esac
    else
        echo ""
        echo "Contact source (for resolving names in chats):"
        echo "  1) iCloud (default — uses your Apple ID)"
        echo "  2) Google Contacts (requires app password)"
        echo "  3) Fastmail"
        echo "  4) Nextcloud"
        echo "  5) Other CardDAV server"
        read -p "Choice [1]: " CONTACT_CHOICE
        CONTACT_CHOICE="${CONTACT_CHOICE:-1}"
        if [ "$CONTACT_CHOICE" != "1" ]; then
            CONFIGURE_CARDDAV=true
        fi
    fi

    if [ "$CONFIGURE_CARDDAV" = true ]; then
        # Show menu if we're changing from an existing provider
        if [ -n "$CURRENT_CARDDAV_EMAIL" ] && [ "$CURRENT_CARDDAV_EMAIL" != '""' ]; then
            echo ""
            echo "  1) iCloud (remove external CardDAV)"
            echo "  2) Google Contacts (requires app password)"
            echo "  3) Fastmail"
            echo "  4) Nextcloud"
            echo "  5) Other CardDAV server"
            read -p "Choice: " CONTACT_CHOICE
        fi

        CARDDAV_EMAIL=""
        CARDDAV_PASSWORD=""
        CARDDAV_USERNAME=""
        CARDDAV_URL=""

        if [ "${CONTACT_CHOICE:-}" = "1" ]; then
            # Remove external CardDAV — clear the config fields
            python3 -c "
import re
text = open('$CONFIG').read()
def patch(text, key, val):
    return re.sub(r'^(\s+' + re.escape(key) + r'\s*:)\s*.*$', r'\1 ' + val, text, count=1, flags=re.MULTILINE)
text = patch(text, 'email', '\"\"')
text = patch(text, 'url', '\"\"')
text = patch(text, 'username', '\"\"')
text = patch(text, 'password_encrypted', '\"\"')
open('$CONFIG', 'w').write(text)
"
            rm -f "$CARDDAV_BACKUP"
            echo "✓ Switched to iCloud contacts"
        elif [ -n "${CONTACT_CHOICE:-}" ]; then
            read -p "Email address: " CARDDAV_EMAIL
            if [ -z "$CARDDAV_EMAIL" ]; then
                echo "ERROR: Email is required." >&2
                exit 1
            fi

            case "$CONTACT_CHOICE" in
                2)
                    CARDDAV_URL="https://www.googleapis.com/carddav/v1/principals/$CARDDAV_EMAIL/lists/default/"
                    echo "  Note: Use a Google App Password, without spaces (https://myaccount.google.com/apppasswords)"
                    ;;
                3)
                    CARDDAV_URL="https://carddav.fastmail.com/dav/addressbooks/user/$CARDDAV_EMAIL/Default/"
                    echo "  Note: Use a Fastmail App Password (Settings → Privacy & Security → App Passwords)"
                    ;;
                4)
                    read -p "Nextcloud server URL (e.g. https://cloud.example.com): " NC_SERVER
                    NC_SERVER="${NC_SERVER%/}"
                    CARDDAV_URL="$NC_SERVER/remote.php/dav"
                    ;;
                5)
                    read -p "CardDAV server URL: " CARDDAV_URL
                    if [ -z "$CARDDAV_URL" ]; then
                        echo "ERROR: URL is required." >&2
                        exit 1
                    fi
                    ;;
            esac

            read -p "Username (leave empty to use email): " CARDDAV_USERNAME
            read -s -p "App password: " CARDDAV_PASSWORD
            echo ""
            if [ -z "$CARDDAV_PASSWORD" ]; then
                echo "ERROR: Password is required." >&2
                exit 1
            fi

            # Encrypt password and patch config
            CARDDAV_ARGS="--email $CARDDAV_EMAIL --password $CARDDAV_PASSWORD --url $CARDDAV_URL"
            if [ -n "$CARDDAV_USERNAME" ]; then
                CARDDAV_ARGS="$CARDDAV_ARGS --username $CARDDAV_USERNAME"
            fi
            CARDDAV_JSON=$("$BINARY" carddav-setup $CARDDAV_ARGS 2>/dev/null) || CARDDAV_JSON=""

            if [ -z "$CARDDAV_JSON" ]; then
                echo "⚠  CardDAV setup failed. You can configure it manually in $CONFIG"
            else
                CARDDAV_RESOLVED_URL=$(echo "$CARDDAV_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['url'])")
                CARDDAV_ENC=$(echo "$CARDDAV_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['password_encrypted'])")
                EFFECTIVE_USERNAME="${CARDDAV_USERNAME:-$CARDDAV_EMAIL}"
                python3 -c "
import re
text = open('$CONFIG').read()
def patch(text, key, val):
    return re.sub(r'^(\s+' + re.escape(key) + r'\s*:)\s*.*$', r'\1 ' + val, text, count=1, flags=re.MULTILINE)
text = patch(text, 'email', '\"$CARDDAV_EMAIL\"')
text = patch(text, 'url', '\"$CARDDAV_RESOLVED_URL\"')
text = patch(text, 'username', '\"$EFFECTIVE_USERNAME\"')
text = patch(text, 'password_encrypted', '\"$CARDDAV_ENC\"')
open('$CONFIG', 'w').write(text)
"
                echo "✓ CardDAV configured: $CARDDAV_EMAIL → $CARDDAV_RESOLVED_URL"
                cat > "$CARDDAV_BACKUP" << BKEOF
SAVED_CARDDAV_EMAIL="$CARDDAV_EMAIL"
SAVED_CARDDAV_URL="$CARDDAV_RESOLVED_URL"
SAVED_CARDDAV_USERNAME="$EFFECTIVE_USERNAME"
SAVED_CARDDAV_ENC="$CARDDAV_ENC"
BKEOF
            fi
        fi
    fi
fi

# ── Check for existing login / prompt if needed ──────────────
DB_URI=$(python3 -c "
import re
text = open('$CONFIG').read()
m = re.search(r'^\s+uri:\s*file:([^?]+)', text, re.MULTILINE)
print(m.group(1) if m else '')
")
NEEDS_LOGIN=false

if [ -z "$DB_URI" ] || [ ! -f "$DB_URI" ]; then
    NEEDS_LOGIN=true
elif command -v sqlite3 >/dev/null 2>&1; then
    LOGIN_COUNT=$(sqlite3 "$DB_URI" "SELECT count(*) FROM user_login;" 2>/dev/null || echo "0")
    if [ "$LOGIN_COUNT" = "0" ]; then
        NEEDS_LOGIN=true
    fi
else
    # sqlite3 not available — can't verify DB has logins, assume login needed
    NEEDS_LOGIN=true
fi

# Require re-login if keychain trust-circle state is missing.
# This catches upgrades from pre-keychain versions where the device-passcode
# step was never run. If trustedpeers.plist exists with a user_identity, the
# keychain was joined successfully and any transient PCS errors are harmless.
SESSION_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/mautrix-imessage"
TRUSTEDPEERS_FILE="$SESSION_DIR/trustedpeers.plist"
FORCE_CLEAR_STATE=false
# Trust-circle only applies to CloudKit backfill — chatdb never creates
# trustedpeers.plist.  Match Go's UseCloudKitBackfill(): cloudkit_backfill
# must be true AND backfill_source must not be "chatdb".
CK_ENABLED=$(awk '/cloudkit_backfill:/{print $2; exit}' "$CONFIG" 2>/dev/null)
BF_SOURCE=$(awk '/backfill_source:/{print $2; exit}' "$CONFIG" 2>/dev/null)
if [ "$NEEDS_LOGIN" = "false" ] && [ "$CK_ENABLED" = "true" ] && [ "$BF_SOURCE" != "chatdb" ]; then
    HAS_CLIQUE=false
    if [ -f "$TRUSTEDPEERS_FILE" ]; then
        if grep -q "<key>userIdentity</key>\|<key>user_identity</key>" "$TRUSTEDPEERS_FILE" 2>/dev/null; then
            HAS_CLIQUE=true
        fi
    fi

    if [ "$HAS_CLIQUE" != "true" ]; then
        echo "⚠ Existing login found, but keychain trust-circle is not initialized."
        echo "  Forcing fresh login so device-passcode step can run."
        NEEDS_LOGIN=true
        FORCE_CLEAR_STATE=true
    fi
fi

if [ "$NEEDS_LOGIN" = "true" ]; then
    echo ""
    echo "┌─────────────────────────────────────────────────┐"
    echo "│  No valid iMessage login found — starting login │"
    echo "└─────────────────────────────────────────────────┘"
    echo ""
    # Stop the bridge if running (otherwise it holds the DB lock)
    launchctl unload "$PLIST" 2>/dev/null || true

    if [ "${FORCE_CLEAR_STATE:-false}" = "true" ]; then
        echo "Clearing stale local state before login..."
        rm -f "$DB_URI" "$DB_URI-wal" "$DB_URI-shm"
        rm -f "$SESSION_DIR/session.json" "$SESSION_DIR/identity.plist" "$SESSION_DIR/trustedpeers.plist"
    fi

    (cd "$DATA_DIR" && "$BINARY" login -c "$CONFIG")
    echo ""
fi

# ── Preferred handle (runs every time, can reconfigure) ────────
HANDLE_BACKUP="$DATA_DIR/.preferred-handle"
CURRENT_HANDLE=$(grep 'preferred_handle:' "$CONFIG" 2>/dev/null | head -1 | sed "s/.*preferred_handle: *//;s/['\"]//g" | tr -d ' ' || true)

# Try to recover from backups if not set in config
if [ -z "$CURRENT_HANDLE" ]; then
    if command -v sqlite3 >/dev/null 2>&1 && [ -n "${DB_URI:-}" ] && [ -f "${DB_URI:-}" ]; then
        CURRENT_HANDLE=$(sqlite3 "$DB_URI" "SELECT json_extract(metadata, '$.preferred_handle') FROM user_login LIMIT 1;" 2>/dev/null || true)
    fi
    if [ -z "$CURRENT_HANDLE" ] && [ -f "$SESSION_DIR/session.json" ] && command -v python3 >/dev/null 2>&1; then
        CURRENT_HANDLE=$(python3 -c "import json; print(json.load(open('$SESSION_DIR/session.json')).get('preferred_handle',''))" 2>/dev/null || true)
    fi
    if [ -z "$CURRENT_HANDLE" ] && [ -f "$HANDLE_BACKUP" ]; then
        CURRENT_HANDLE=$(cat "$HANDLE_BACKUP")
    fi
fi

# Skip interactive prompt if login just ran (login flow already asked)
if [ -t 0 ] && [ "$NEEDS_LOGIN" = "false" ]; then
    # Get available handles from session state (available after login)
    AVAILABLE_HANDLES=$("$BINARY" list-handles 2>/dev/null | grep -E '^(tel:|mailto:)' || true)
    if [ -n "$AVAILABLE_HANDLES" ]; then
        echo ""
        echo "Preferred handle (your iMessage sender address):"
        i=1
        declare -a HANDLE_LIST=()
        while IFS= read -r h; do
            MARKER=""
            if [ "$h" = "$CURRENT_HANDLE" ]; then
                MARKER=" (current)"
            fi
            echo "  $i) $h$MARKER"
            HANDLE_LIST+=("$h")
            i=$((i + 1))
        done <<< "$AVAILABLE_HANDLES"

        if [ -n "$CURRENT_HANDLE" ]; then
            read -p "Choice [keep current]: " HANDLE_CHOICE
        else
            read -p "Choice [1]: " HANDLE_CHOICE
        fi

        if [ -n "$HANDLE_CHOICE" ]; then
            if [ "$HANDLE_CHOICE" -ge 1 ] 2>/dev/null && [ "$HANDLE_CHOICE" -le "${#HANDLE_LIST[@]}" ] 2>/dev/null; then
                CURRENT_HANDLE="${HANDLE_LIST[$((HANDLE_CHOICE - 1))]}"
            fi
        elif [ -z "$CURRENT_HANDLE" ] && [ ${#HANDLE_LIST[@]} -gt 0 ]; then
            CURRENT_HANDLE="${HANDLE_LIST[0]}"
        fi
    elif [ -n "$CURRENT_HANDLE" ]; then
        echo ""
        echo "Preferred handle: $CURRENT_HANDLE"
        read -p "New handle, or Enter to keep current: " NEW_HANDLE
        if [ -n "$NEW_HANDLE" ]; then
            CURRENT_HANDLE="$NEW_HANDLE"
        fi
    fi
fi

# Write preferred handle to config (add key if missing, patch if present)
if [ -n "${CURRENT_HANDLE:-}" ]; then
    if grep -q 'preferred_handle:' "$CONFIG" 2>/dev/null; then
        sed -i '' "s|preferred_handle: .*|preferred_handle: '$CURRENT_HANDLE'|" "$CONFIG"
    else
        sed -i '' "/^network:/a\\
\\    preferred_handle: '$CURRENT_HANDLE'
" "$CONFIG"
    fi
    echo "✓ Preferred handle: $CURRENT_HANDLE"
    echo "$CURRENT_HANDLE" > "$HANDLE_BACKUP"
fi

# ── Ensure video_transcoding key exists in config ──────────────
if ! grep -q 'video_transcoding:' "$CONFIG" 2>/dev/null; then
    sed -i '' '/cloudkit_backfill:/i\
    video_transcoding: false' "$CONFIG"
fi

# ── Video transcoding (ffmpeg) ─────────────────────────────────
CURRENT_VIDEO_TRANSCODING=$(grep 'video_transcoding:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*video_transcoding: *//' || true)
if [ -t 0 ]; then
    echo ""
    echo "Video Transcoding:"
    echo "  When enabled, non-MP4 videos (e.g. QuickTime .mov) are automatically"
    echo "  converted to MP4 for broad Matrix client compatibility."
    echo "  Requires ffmpeg."
    echo ""
    if [ "$CURRENT_VIDEO_TRANSCODING" = "true" ]; then
        read -p "Enable video transcoding/remuxing? [Y/n]: " ENABLE_VT
        case "$ENABLE_VT" in
            [nN]*)
                sed -i '' "s/video_transcoding: .*/video_transcoding: false/" "$CONFIG"
                echo "✓ Video transcoding disabled"
                ;;
            *)
                if ! command -v ffmpeg >/dev/null 2>&1; then
                    echo "  ffmpeg not found — installing via Homebrew..."
                    brew install ffmpeg
                fi
                echo "✓ Video transcoding enabled"
                ;;
        esac
    else
        read -p "Enable video transcoding/remuxing? [y/N]: " ENABLE_VT
        case "$ENABLE_VT" in
            [yY]*)
                sed -i '' "s/video_transcoding: .*/video_transcoding: true/" "$CONFIG"
                if ! command -v ffmpeg >/dev/null 2>&1; then
                    echo "  ffmpeg not found — installing via Homebrew..."
                    brew install ffmpeg
                fi
                echo "✓ Video transcoding enabled"
                ;;
            *)
                echo "✓ Video transcoding disabled"
                ;;
        esac
    fi
fi

# ── Ensure disable_facetime key exists in config ──────────────
if ! grep -q 'disable_facetime:' "$CONFIG" 2>/dev/null; then
    sed -i '' '/video_transcoding:/a\
    disable_facetime: false' "$CONFIG"
fi

# ── Disable FaceTime Bridge (use native Apple FT instead) ────────
CURRENT_DISABLE_FT=$(grep 'disable_facetime:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*disable_facetime: *//' || true)
if [ -n "${DISABLE_FACETIME:-}" ]; then
    case "$DISABLE_FACETIME" in
        1|true|TRUE|yes|YES)
            sed -i '' "s/disable_facetime: .*/disable_facetime: true/" "$CONFIG"
            echo "✓ FaceTime Bridge disabled (DISABLE_FACETIME env)"
            ;;
        *)
            sed -i '' "s/disable_facetime: .*/disable_facetime: false/" "$CONFIG"
            echo "✓ FaceTime Bridge enabled (DISABLE_FACETIME env)"
            ;;
    esac
elif [ -t 0 ]; then
    echo ""
    echo "FaceTime Bridge:"
    echo "  If you have an Apple device that already handles FaceTime, the"
    echo "  bridge's FT wrapper just clutters your chat. Disable it to skip"
    echo "  !im facetime commands and inbound FT notices."
    echo ""
    if [ "$CURRENT_DISABLE_FT" = "true" ]; then
        read -p "Disable FaceTime Bridge? [Y/n]:" DIS_FT
        case "$DIS_FT" in
            [nN]*)
                sed -i '' "s/disable_facetime: .*/disable_facetime: false/" "$CONFIG"
                echo "✓ FaceTime Bridge enabled"
                ;;
            *)
                echo "✓ FaceTime Bridge disabled"
                ;;
        esac
    else
        read -p "Disable FaceTime Bridge (use native Apple FT)? [y/N]: " DIS_FT
        case "$DIS_FT" in
            [yY]*)
                sed -i '' "s/disable_facetime: .*/disable_facetime: true/" "$CONFIG"
                echo "✓ FaceTime Bridge disabled"
                ;;
            *)
                echo "✓ FaceTime Bridge enabled"
                ;;
        esac
    fi
fi

# ── Ensure heic_conversion key exists in config ──────────────
if ! grep -q 'heic_conversion:' "$CONFIG" 2>/dev/null; then
    sed -i '' '/video_transcoding:/a\
    heic_conversion: false' "$CONFIG"
fi

# ── HEIC conversion (libheif) ─────────────────────────────────
CURRENT_HEIC_CONVERSION=$(grep 'heic_conversion:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*heic_conversion: *//' || true)
if [ -t 0 ]; then
    echo ""
    echo "HEIC Conversion:"
    echo "  When enabled, HEIC/HEIF images are automatically converted to JPEG"
    echo "  for broad Matrix client compatibility."
    echo "  Requires libheif."
    echo ""
    if [ "$CURRENT_HEIC_CONVERSION" = "true" ]; then
        read -p "Enable HEIC to JPEG conversion? [Y/n]: " ENABLE_HC
        case "$ENABLE_HC" in
            [nN]*)
                sed -i '' "s/heic_conversion: .*/heic_conversion: false/" "$CONFIG"
                echo "✓ HEIC conversion disabled"
                ;;
            *)
                if command -v brew >/dev/null 2>&1; then
                    brew list libheif >/dev/null 2>&1 || brew install libheif
                fi
                echo "✓ HEIC conversion enabled"
                ;;
        esac
    else
        read -p "Enable HEIC to JPEG conversion? [y/N]: " ENABLE_HC
        case "$ENABLE_HC" in
            [yY]*)
                sed -i '' "s/heic_conversion: .*/heic_conversion: true/" "$CONFIG"
                if command -v brew >/dev/null 2>&1; then
                    brew list libheif >/dev/null 2>&1 || brew install libheif
                fi
                echo "✓ HEIC conversion enabled"
                ;;
            *)
                echo "✓ HEIC conversion disabled"
                ;;
        esac
    fi
fi

# ── HEIC JPEG quality (only if HEIC conversion is enabled) ───
HEIC_ENABLED=$(grep 'heic_conversion:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*heic_conversion: *//' || true)
if [ "$HEIC_ENABLED" = "true" ]; then
    if ! grep -q 'heic_jpeg_quality:' "$CONFIG" 2>/dev/null; then
        sed -i '' "$(printf '/heic_conversion:/a\\\n    heic_jpeg_quality: 95')" "$CONFIG"
    fi
else
    sed -i '' '/heic_jpeg_quality:/d' "$CONFIG"
fi
if [ "$HEIC_ENABLED" = "true" ] && [ -t 0 ]; then
    CURRENT_QUALITY=$(grep 'heic_jpeg_quality:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*heic_jpeg_quality: *//' || echo "95")
    [ -z "$CURRENT_QUALITY" ] && CURRENT_QUALITY=95
    echo ""
    read -p "JPEG quality for HEIC conversion (1–100) [$CURRENT_QUALITY]: " NEW_QUALITY
    if [ -n "$NEW_QUALITY" ]; then
        if [ "$NEW_QUALITY" -ge 1 ] 2>/dev/null && [ "$NEW_QUALITY" -le 100 ] 2>/dev/null; then
            sed -i '' "s/heic_jpeg_quality: .*/heic_jpeg_quality: $NEW_QUALITY/" "$CONFIG"
            echo "✓ JPEG quality set to $NEW_QUALITY"
        else
            echo "  ⚠ Invalid quality '$NEW_QUALITY' — keeping $CURRENT_QUALITY"
        fi
    else
        echo "✓ JPEG quality: $CURRENT_QUALITY"
    fi
fi

# ── Install LaunchAgent ───────────────────────────────────────
CONFIG_ABS="$(cd "$DATA_DIR" && pwd)/config.yaml"
DATA_ABS="$(cd "$DATA_DIR" && pwd)"
LOG_OUT="$DATA_ABS/bridge.stdout.log"
LOG_ERR="$DATA_ABS/bridge.stderr.log"

mkdir -p "$(dirname "$PLIST")"
launchctl unload "$PLIST" 2>/dev/null || true

cat > "$PLIST" << PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$BUNDLE_ID</string>
    <key>ProgramArguments</key>
    <array>
        <string>$BINARY</string>
        <string>-c</string>
        <string>$CONFIG_ABS</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$DATA_ABS</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$LOG_OUT</string>
    <key>StandardErrorPath</key>
    <string>$LOG_ERR</string>
</dict>
</plist>
PLIST_EOF

launchctl load "$PLIST"
echo "✓ Bridge started (LaunchAgent installed)"
echo ""

# ── Wait for bridge to connect ────────────────────────────────
echo "Waiting for bridge to start..."
for i in $(seq 1 15); do
    if grep -q "Bridge started" "$LOG_OUT" 2>/dev/null; then
        echo "✓ Bridge is running"
        break
    fi
    sleep 1
done

echo ""
echo "═══════════════════════════════════════════════"
echo "  Setup Complete"
echo "═══════════════════════════════════════════════"
echo ""
echo "  Logs:    tail -f $LOG_OUT"
echo "  Restart: launchctl kickstart -k gui/$(id -u)/$BUNDLE_ID"
echo "  Stop:    launchctl bootout gui/$(id -u)/$BUNDLE_ID"
echo ""
