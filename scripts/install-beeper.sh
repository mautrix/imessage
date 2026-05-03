#!/bin/bash
set -euo pipefail

BINARY="$1"
DATA_DIR="$2"
BUNDLE_ID="$3"
PREBUILT_BBCTL="${4:-}"

BRIDGE_NAME="${BRIDGE_NAME:-sh-imessage}"

BINARY="$(cd "$(dirname "$BINARY")" && pwd)/$(basename "$BINARY")"
CONFIG="$DATA_DIR/config.yaml"
PLIST="$HOME/Library/LaunchAgents/$BUNDLE_ID.plist"

# Where we build/cache bbctl (sparse clone — only cmd/bbctl/)
BBCTL_DIR="${BBCTL_DIR:-$HOME/.local/share/mautrix-imessage/bbctl}"
BBCTL_REPO="${BBCTL_REPO:-https://github.com/lrhodin/imessage.git}"
BBCTL_BRANCH="${BBCTL_BRANCH:-master}"

echo ""
echo "═══════════════════════════════════════════════"
echo "  iMessage Bridge Setup (Beeper)"
echo "═══════════════════════════════════════════════"
echo ""

# ── Stop any running bridge instance immediately ──────────────
# Do this before any setup work so the bridge isn't running while we ask
# questions, patch config, or run init-db. On re-setup scenarios (bbctl delete),
# systemd/LaunchAgent may have restarted the bridge — stop it now.
launchctl bootout "gui/$(id -u)/$BUNDLE_ID" 2>/dev/null || true

# ── Permission repair helper ──────────────────────────────────
# Detects and fixes broken permissions in config.yaml. Matches the same
# patterns as repairPermissions() / fixPermissionsOnDisk() in the Go code:
#   - Empty username: "@:beeper.com", "@": ...
#   - Example defaults: "@admin:example.com", "example.com", "*": relay
# Usage: fix_permissions <config_path> <username>
fix_permissions() {
    local config="$1" whoami="$2"
    if grep -q '"@:\|"@":\|@.*example\.com\|"\*":.*relay' "$config" 2>/dev/null; then
        local mxid="@${whoami}:beeper.com"
        sed -i '' '/permissions:/,/^[^ ]/{
            s/"@[^"]*": admin/"'"$mxid"'": admin/
            /@.*example\.com/d
            /"\*":.*relay/d
            /"@":/d
            /"@:/d
        }' "$config"
        return 0
    fi
    return 1
}

# ── Build bbctl from source ───────────────────────────────────
BBCTL="$BBCTL_DIR/bbctl"

# Warn about old full-repo clone and offer to remove it
OLD_BBCTL_DIR="$HOME/.local/share/mautrix-imessage/bridge-manager"
if [ -d "$OLD_BBCTL_DIR/.git" ] && [ "$BBCTL_DIR" != "$OLD_BBCTL_DIR" ]; then
    echo "⚠  Found old full-repo clone at $OLD_BBCTL_DIR"
    echo "   This is no longer needed (bbctl now uses a sparse checkout)."
    if [ -t 0 ]; then
        read -p "   Delete it to free disk space? [Y/n]: " DEL_OLD
        case "$DEL_OLD" in
            [nN]*) ;;
            *)     rm -rf "$OLD_BBCTL_DIR"
                   echo "   ✓ Removed $OLD_BBCTL_DIR" ;;
        esac
    else
        echo "   You can safely delete it: rm -rf $OLD_BBCTL_DIR"
    fi
fi

build_bbctl() {
    echo "Building bbctl..."
    mkdir -p "$(dirname "$BBCTL_DIR")"
    if [ -d "$BBCTL_DIR/.git" ]; then
        cd "$BBCTL_DIR"
        git fetch --quiet origin
        git reset --hard --quiet "origin/$BBCTL_BRANCH"
    else
        rm -rf "$BBCTL_DIR"
        git clone --filter=blob:none --no-checkout --quiet \
            --branch "$BBCTL_BRANCH" "$BBCTL_REPO" "$BBCTL_DIR"
        cd "$BBCTL_DIR"
        git sparse-checkout init --cone
        git sparse-checkout set cmd/bbctl
        git checkout --quiet "$BBCTL_BRANCH"
    fi
    go build -o bbctl ./cmd/bbctl/ 2>&1
    cd - >/dev/null
    echo "✓ Built bbctl"
}

if [ -n "$PREBUILT_BBCTL" ] && [ -x "$PREBUILT_BBCTL" ]; then
    # Install the bbctl built by `make build` into BBCTL_DIR
    mkdir -p "$BBCTL_DIR"
    cp "$PREBUILT_BBCTL" "$BBCTL"
    echo "✓ Installed bbctl to $BBCTL_DIR/"
elif [ ! -x "$BBCTL" ]; then
    build_bbctl
else
    echo "✓ Found bbctl: $BBCTL"
    # Update if repo has changes
    if [ -d "$BBCTL_DIR/.git" ]; then
        cd "$BBCTL_DIR"
        git fetch --quiet origin 2>/dev/null || true
        LOCAL=$(git rev-parse HEAD 2>/dev/null)
        REMOTE=$(git rev-parse "origin/$BBCTL_BRANCH" 2>/dev/null || echo "$LOCAL")
        cd - >/dev/null
        if [ "$LOCAL" != "$REMOTE" ]; then
            echo "  Updating bbctl..."
            build_bbctl
        fi
    fi
fi

# ── Check bbctl login ────────────────────────────────────────
if ! "$BBCTL" whoami >/dev/null 2>&1 || "$BBCTL" whoami 2>&1 | grep -qi "not logged in"; then
    echo ""
    echo "Not logged into Beeper. Running bbctl login..."
    echo ""
    "$BBCTL" login
fi
# Capture username (discard stderr so "Fetching whoami..." doesn't contaminate)
WHOAMI=$("$BBCTL" whoami 2>/dev/null | head -1 || true)
# On slow machines the Beeper API may not have the username ready yet — retry
if [ -z "$WHOAMI" ] || [ "$WHOAMI" = "null" ]; then
    for i in 1 2 3 4 5; do
        echo "  Waiting for username from Beeper API (attempt $i/5)..."
        sleep 3
        WHOAMI=$("$BBCTL" whoami 2>/dev/null | head -1 || true)
        [ -n "$WHOAMI" ] && [ "$WHOAMI" != "null" ] && break
    done
fi
if [ -z "$WHOAMI" ] || [ "$WHOAMI" = "null" ]; then
    echo ""
    echo "ERROR: Could not get username from Beeper API."
    echo "  This can happen when the API is slow to propagate after login."
    echo "  Wait a minute and re-run: make install-beeper"
    exit 1
fi
echo "✓ Logged in: $WHOAMI"

# ── Check for existing bridge registration ────────────────────
# If the bridge is already registered on the server but we're about to
# generate a fresh config (no local config file), the old registration's
# rooms would be orphaned.  Delete it first so the server cleans up rooms.
EXISTING_BRIDGE=$("$BBCTL" whoami 2>&1 | grep "^\s*$BRIDGE_NAME " || true)
if [ -n "$EXISTING_BRIDGE" ] && [ ! -f "$CONFIG" ]; then
    echo ""
    echo "⚠  Found existing '$BRIDGE_NAME' registration on server but no local config."
    echo "   Deleting old registration to avoid orphaned rooms..."
    "$BBCTL" delete "$BRIDGE_NAME"
    echo "✓ Old registration cleaned up"
    echo "   Waiting for server-side deletion to complete..."
    sleep 5
fi

# ── Generate config via bbctl ─────────────────────────────────
mkdir -p "$DATA_DIR"
if [ -f "$CONFIG" ] && [ -z "$EXISTING_BRIDGE" ]; then
    # Config exists locally but bridge isn't registered on server (e.g. bbctl
    # delete was run manually).  The stale config has an invalid as_token and
    # the DB references rooms that no longer exist.
    #
    # Double-check by retrying bbctl whoami — a transient network error or the
    # bridge restarting can cause the first check to return empty even though
    # the registration is fine.
    echo "⚠  Bridge not found in bbctl whoami — retrying in 3s to rule out transient error..."
    sleep 3
    EXISTING_BRIDGE=$("$BBCTL" whoami 2>&1 | grep "^\s*$BRIDGE_NAME " || true)
    if [ -z "$EXISTING_BRIDGE" ]; then
        echo "⚠  Local config exists but bridge is not registered on server."
        echo "   Removing stale config and database to re-register..."
        rm -f "$CONFIG"
        DB_DIR="$(cd "$DATA_DIR" && pwd)"
        rm -f "$DB_DIR"/mautrix-imessage.db*
    else
        echo "✓ Bridge found on retry — keeping existing config and database"
    fi
fi
if [ -f "$CONFIG" ]; then
    echo "✓ Config already exists at $CONFIG"
    echo "  Delete it to regenerate from Beeper."
else
    echo "Generating Beeper config..."
    for attempt in 1 2 3 4 5; do
        if "$BBCTL" config --type imessage-v2 -o "$CONFIG" "$BRIDGE_NAME" 2>&1; then
            break
        fi
        if [ "$attempt" -eq 5 ]; then
            echo "ERROR: Failed to register appservice after $attempt attempts."
            exit 1
        fi
        echo "  Retrying in 5s... (attempt $attempt/5)"
        sleep 5
    done
    # Make DB path absolute so it doesn't depend on working directory
    DATA_ABS_TMP="$(cd "$DATA_DIR" && pwd)"
    sed -i '' "s|uri: file:mautrix-imessage.db|uri: file:$DATA_ABS_TMP/mautrix-imessage.db|" "$CONFIG"
    # iMessage CloudKit chats can have tens of thousands of messages.
    # Deliver all history in one forward batch to avoid DAG fragmentation.
    sed -i '' 's/max_initial_messages: [0-9]*/max_initial_messages: 2147483647/' "$CONFIG"
    sed -i '' 's/max_catchup_messages: [0-9]*/max_catchup_messages: 5000/' "$CONFIG"
    sed -i '' 's/batch_size: [0-9]*/batch_size: 10000/' "$CONFIG"
    # Enable unlimited backward backfill (default is 0 which disables it)
    sed -i '' 's/max_batches: 0$/max_batches: -1/' "$CONFIG"
    # Use 1s between batches — fast enough for backfill, prevents idle hot-loop
    sed -i '' 's/batch_delay: [0-9]*/batch_delay: 1/' "$CONFIG"

    echo "✓ Config saved to $CONFIG"
fi

# No bridge-state override needed here — the bridge will post its own
# state when it actually starts at the end of setup.

# ── Belt-and-suspenders: fix broken permissions ───────────────
if [ -n "$WHOAMI" ] && [ "$WHOAMI" != "null" ]; then
    if fix_permissions "$CONFIG" "$WHOAMI"; then
        echo "✓ Fixed permissions: @${WHOAMI}:beeper.com → admin"
    fi
else
    if grep -q '"@:\|"@":\|@.*example\.com' "$CONFIG" 2>/dev/null; then
        echo ""
        echo "ERROR: Config has broken permissions and cannot determine your username."
        echo "  Try: $BBCTL login && rm $CONFIG && re-run make install-beeper"
        echo ""
        exit 1
    fi
fi

# Ensure backfill settings are sane for existing configs
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

if ! grep -q "beeper" "$CONFIG" 2>/dev/null; then
    echo ""
    echo "WARNING: Config doesn't appear to contain Beeper details."
    echo "  Try: rm $CONFIG && re-run make install-beeper"
    echo ""
    exit 1
fi

# ── Ensure cloudkit_backfill key exists in config ─────────────
if ! grep -q 'cloudkit_backfill:' "$CONFIG" 2>/dev/null; then
    # Insert after initial_sync_days if it exists (old configs), otherwise append
    if grep -q 'initial_sync_days:' "$CONFIG" 2>/dev/null; then
        sed -i '' '/initial_sync_days:/a\
    cloudkit_backfill: false' "$CONFIG"
    else
        echo "    cloudkit_backfill: false" >> "$CONFIG"
    fi
fi

# ── Ensure backfill_source key exists in config ───────────────
if ! grep -q 'backfill_source:' "$CONFIG" 2>/dev/null; then
    sed -i '' '/cloudkit_backfill:/a\
    backfill_source: cloudkit' "$CONFIG"
fi

# ── Backfill source selection ─────────────────────────────────
# On first run (fresh DB), show a 3-way prompt. On re-runs, preserve existing.
DB_PATH_CHECK=$(grep 'uri:' "$CONFIG" | head -1 | sed 's/.*uri: file://' | sed 's/?.*//')
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
                ;;
        esac
    else
        # Re-run: show current setting, allow changing CloudKit on/off
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
    DB_PATH=$(grep 'uri:' "$CONFIG" | head -1 | sed 's/.*uri: file://' | sed 's/?.*//')
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
    if [ "$PATCHED_BACKFILL" = true ]; then
        echo "✓ Updated backfill settings (max_initial=unlimited, batch_size=10000, max_batches=-1)"
    fi
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
if 'carddav:' not in text:
    lines = text.split('\\n')
    insert_at = len(lines)
    in_network = False
    for i, line in enumerate(lines):
        if line.startswith('network:'):
            in_network = True
            continue
        if in_network and line and not line[0].isspace() and not line.startswith('#'):
            insert_at = i
            break
    carddav = ['    carddav:', '        email: \"\"', '        url: \"\"', '        username: \"\"', '        password_encrypted: \"\"']
    lines = lines[:insert_at] + carddav + lines[insert_at:]
    text = '\\n'.join(lines)
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
if 'carddav:' not in text:
    lines = text.split('\\n')
    insert_at = len(lines)
    in_network = False
    for i, line in enumerate(lines):
        if line.startswith('network:'):
            in_network = True
            continue
        if in_network and line and not line[0].isspace() and not line.startswith('#'):
            insert_at = i
            break
    carddav = ['    carddav:', '        email: \"\"', '        url: \"\"', '        username: \"\"', '        password_encrypted: \"\"']
    lines = lines[:insert_at] + carddav + lines[insert_at:]
    text = '\\n'.join(lines)
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
DB_URI=$(grep 'uri:' "$CONFIG" | head -1 | sed 's/.*uri: file://' | sed 's/?.*//')
NEEDS_LOGIN=false

SESSION_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/mautrix-imessage"
SESSION_FILE="$SESSION_DIR/session.json"

# ── Brief init start (fresh install only) ────────────────────
# On a fresh install with no prior session, start the bridge briefly so it
# creates the DB schema and appears in Beeper as "stopped" during setup.
# We kill it immediately — all config questions (video, HEIC, handle) and
# the iCloud sync gate are answered next, THEN Apple login (APNs) happens
# at the very end so no messages are buffered before the bridge is ready.
if [ "$IS_FRESH_DB" = "true" ]; then
    echo ""
    echo "Initializing bridge database..."
    if ! (cd "$DATA_DIR" && "$BINARY" init-db -c "$CONFIG"); then
        echo "✗ Bridge database initialization failed — check the output above for details"
        exit 1
    fi
    echo "✓ Bridge database initialized — answering setup questions"
fi

# ── Ensure bridge is stopped during setup ─────────────────────
# bbctl config posts StateStarting which makes Beeper show "Running".
# Stopping the LaunchAgent disconnects the websocket, which makes
# Beeper detect it as unreachable and overrides the stale state.
launchctl bootout "gui/$(id -u)/$BUNDLE_ID" 2>/dev/null || true

if [ -z "$DB_URI" ] || [ ! -f "$DB_URI" ]; then
    # DB missing — check if session.json can auto-restore (has hardware_key for Linux, or macOS)
    if [ -f "$SESSION_FILE" ] && { grep -q '"hardware_key"' "$SESSION_FILE" 2>/dev/null || [ "$(uname -s)" = "Darwin" ]; }; then
        echo "✓ No database yet, but session state found — bridge will auto-restore login"
        NEEDS_LOGIN=false
    else
        NEEDS_LOGIN=true
    fi
elif command -v sqlite3 >/dev/null 2>&1; then
    LOGIN_COUNT=$(sqlite3 "$DB_URI" "SELECT count(*) FROM user_login;" 2>/dev/null || echo "0")
    if [ "$LOGIN_COUNT" = "0" ]; then
        if [ -f "$SESSION_FILE" ] && { grep -q '"hardware_key"' "$SESSION_FILE" 2>/dev/null || [ "$(uname -s)" = "Darwin" ]; }; then
            echo "✓ No login in database, but session state found — bridge will auto-restore"
            NEEDS_LOGIN=false
        else
            NEEDS_LOGIN=true
        fi
    fi
else
    NEEDS_LOGIN=true
fi

# Require re-login if keychain trust-circle state is missing.
# This catches upgrades from pre-keychain versions where the device-passcode
# step was never run. If trustedpeers.plist exists with a user_identity, the
# keychain was joined successfully and any transient PCS errors are harmless.
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

# ── Ensure statuskit_notifications key exists in config ─────
if ! grep -q 'statuskit_notifications:' "$CONFIG" 2>/dev/null; then
    sed -i '' '/disable_facetime:/a\
    statuskit_notifications: true' "$CONFIG"
fi

# ── StatusKit notifications (iOS 18 Focus / DND inline notices) ───
CURRENT_STATUSKIT_NOTIF=$(grep 'statuskit_notifications:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*statuskit_notifications: *//' || true)
if [ -n "${STATUSKIT_NOTIFICATIONS:-}" ]; then
    case "$STATUSKIT_NOTIFICATIONS" in
        1|true|TRUE|yes|YES)
            sed -i '' "s/statuskit_notifications: .*/statuskit_notifications: true/" "$CONFIG"
            echo "✓ StatusKit notifications enabled (STATUSKIT_NOTIFICATIONS env)"
            ;;
        *)
            sed -i '' "s/statuskit_notifications: .*/statuskit_notifications: false/" "$CONFIG"
            echo "✓ StatusKit notifications disabled (STATUSKIT_NOTIFICATIONS env)"
            ;;
    esac
elif [ -t 0 ]; then
    echo ""
    echo "StatusKit notifications:"
    echo "  When a contact enables iOS 18 Focus or Do Not Disturb on their"
    echo "  iPhone, the bridge can post a silent notice in the DM portal"
    echo "  (\"🔕 Name has notifications silenced (Do Not Disturb).\") and"
    echo "  update Matrix ghost presence — the same affordance Apple's"
    echo "  Messages app shows in-conversation. Disabling keeps the"
    echo "  StatusKit registration intact but suppresses the notices."
    echo ""
    if [ "$CURRENT_STATUSKIT_NOTIF" = "false" ]; then
        read -p "Enable StatusKit notifications? [y/N]: " EN_SK
        case "$EN_SK" in
            [yY]*)
                sed -i '' "s/statuskit_notifications: .*/statuskit_notifications: true/" "$CONFIG"
                echo "✓ StatusKit notifications enabled"
                ;;
            *)
                echo "✓ StatusKit notifications disabled"
                ;;
        esac
    else
        read -p "Enable StatusKit notifications? [Y/n]: " EN_SK
        case "$EN_SK" in
            [nN]*)
                sed -i '' "s/statuskit_notifications: .*/statuskit_notifications: false/" "$CONFIG"
                echo "✓ StatusKit notifications disabled"
                ;;
            *)
                echo "✓ StatusKit notifications enabled"
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

# ── iCloud sync gate (CloudKit + fresh DB) ───────────────────
# Runs before Apple login so that iCloud is fully synced before APNs first
# connects.  This ensures CloudKit backfill can deduplicate any messages that
# Apple buffers and delivers the moment the bridge registers with APNs.
_ck_backfill=$(grep 'cloudkit_backfill:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*cloudkit_backfill: *//' || true)
_ck_source=$(grep 'backfill_source:' "$CONFIG" 2>/dev/null | head -1 | sed 's/.*backfill_source: *//' || true)
if [ "$IS_FRESH_DB" = "true" ] && [ "$_ck_backfill" = "true" ] && [ "$_ck_source" != "chatdb" ] && [ -t 0 ]; then
    echo ""
    echo "┌─────────────────────────────────────────────────────────────┐"
    echo "│  Last step: sync iCloud Messages before starting            │"
    echo "│                                                             │"
    echo "│  On your iPhone, iPad, Mac, or OpenBubbles:                 │"
    echo "│    Settings → [Your Name] → iCloud → Messages → Sync Now    │"
    echo "│                                                             │"
    echo "│  Wait for sync to complete, then press Y to start.          │"
    echo "└─────────────────────────────────────────────────────────────┘"
    echo ""
    read -p "Have you synced iCloud Messages and are ready to start? [y/N]: " _sync_ready
    case "$_sync_ready" in
        [yY]*) echo "✓ Starting bridge" ;;
        *)
            echo ""
            echo "Re-run 'make install-beeper' after syncing iCloud Messages."
            exit 0
            ;;
    esac
fi

# ── Apple login (APNs connects here — after all questions) ───
# check-restore runs first: if session.json + keystore are intact, no login needed.
if [ "$NEEDS_LOGIN" = "true" ] && [ "${FORCE_CLEAR_STATE:-false}" != "true" ] && "$BINARY" check-restore 2>/dev/null; then
    echo "✓ Backup session state validated — bridge will auto-restore login"
    NEEDS_LOGIN=false
fi

LOGIN_RAN=false
if [ "$NEEDS_LOGIN" = "true" ]; then
    LOGIN_RAN=true
    echo ""
    echo "┌─────────────────────────────────────────────────┐"
    echo "│  No valid iMessage login found — starting login │"
    echo "└─────────────────────────────────────────────────┘"
    echo ""
    # Stop the bridge if running (otherwise it holds the DB lock)
    GUI_DOMAIN_TMP="gui/$(id -u)"
    launchctl bootout "$GUI_DOMAIN_TMP/$BUNDLE_ID" 2>/dev/null || true

    if [ "${FORCE_CLEAR_STATE:-false}" = "true" ]; then
        echo "Clearing stale local state before login..."
        rm -f "$DB_URI" "$DB_URI-wal" "$DB_URI-shm"
        rm -f "$SESSION_DIR/session.json" "$SESSION_DIR/identity.plist" "$SESSION_DIR/trustedpeers.plist"
    fi

    # Run login from the data directory so the keystore (state/keystore.plist)
    # is written to the same location the launchd service will read from.
    (cd "$DATA_DIR" && "$BINARY" login -n -c "$CONFIG")
    echo ""

    # Re-check permissions after login — the config upgrader may have
    # corrupted them even with -n if repairPermissions couldn't determine
    # the username.
    if [ -n "$WHOAMI" ] && [ "$WHOAMI" != "null" ]; then
        if fix_permissions "$CONFIG" "$WHOAMI"; then
            echo "✓ Fixed permissions after login: @${WHOAMI}:beeper.com → admin"
        fi
    fi
fi

# ── Stop bridge before applying config changes ────────────────
launchctl bootout "gui/$(id -u)/$BUNDLE_ID" 2>/dev/null || true

# ── Optional shell shortcuts (asked before preferred handle so the
#    handle prompt remains the last interactive step) ─────────────
# LOG_OUT and GUI_DOMAIN aren't computed until later in this script;
# derive them inline here using the same formulas used below.
_SHORTCUT_DATA_ABS="$(cd "$DATA_DIR" && pwd)"
_SHORTCUT_LOG_OUT="$_SHORTCUT_DATA_ABS/bridge.stdout.log"
_SHORTCUT_GUI_DOMAIN="gui/$(id -u)"

echo ""
echo "Want easy commands you can type from any terminal to control the bridge?"
echo "  start-imessage     stop-imessage     restart-imessage     imessage-log"
read -r -p "Add them? [y/N]: " _shortcut_ans
case "$_shortcut_ans" in
    [yY]|[yY][eE][sS])
        case "$SHELL" in
            */zsh)  RC_FILE="$HOME/.zshrc" ;;
            */bash) RC_FILE="$HOME/.bashrc" ;;
            *)      RC_FILE="" ;;
        esac
        if [ -z "$RC_FILE" ]; then
            echo "  Couldn't detect your shell from \$SHELL ($SHELL) — skipping. (Bash and Zsh are supported.)"
        else
            MARKER_START="# >>> mautrix-imessage shortcuts (managed) >>>"
            MARKER_END="# <<< mautrix-imessage shortcuts (managed) <<<"
            if [ -f "$RC_FILE" ] && grep -qF "$MARKER_START" "$RC_FILE"; then
                awk -v s="$MARKER_START" -v e="$MARKER_END" '
                    $0 == s { skip = 1; next }
                    $0 == e { skip = 0; next }
                    !skip   { print }
                ' "$RC_FILE" > "$RC_FILE.tmp" && mv "$RC_FILE.tmp" "$RC_FILE"
            fi
            cat >> "$RC_FILE" <<EOF
$MARKER_START
alias start-imessage='launchctl bootstrap $_SHORTCUT_GUI_DOMAIN $PLIST'
alias stop-imessage='launchctl bootout $_SHORTCUT_GUI_DOMAIN/$BUNDLE_ID'
alias restart-imessage='launchctl kickstart -k $_SHORTCUT_GUI_DOMAIN/$BUNDLE_ID'
alias imessage-log='tail -f $_SHORTCUT_LOG_OUT'
$MARKER_END
EOF
            echo "  ✓ Shortcuts added. Open a new terminal (or run \`source $RC_FILE\` here) and you can type:"
            echo "      start-imessage   stop-imessage   restart-imessage   imessage-log"
        fi
        ;;
    *) echo "  Skipped — re-run this installer to add them later." ;;
esac
echo ""

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

# Skip handle prompt if login just ran and already set a handle (the login
# flow on macOS asks for handle during Apple ID auth — no need to ask twice).
# On Linux (external-key flow), login doesn't ask, so CURRENT_HANDLE is empty
# and the prompt still shows.
if [ -t 0 ] && { [ "$LOGIN_RAN" != "true" ] || [ -z "$CURRENT_HANDLE" ]; }; then
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
    else
        # list-handles returned empty (e.g. session not yet populated).
        # Fall back to manual entry so the bridge doesn't start without a handle.
        echo ""
        echo "Could not detect handles automatically."
        read -p "Enter your iMessage handle (e.g. tel:+12345678900 or mailto:you@icloud.com): " CURRENT_HANDLE
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

# ── Install LaunchAgent ───────────────────────────────────────
CONFIG_ABS="$(cd "$DATA_DIR" && pwd)/config.yaml"
DATA_ABS="$(cd "$DATA_DIR" && pwd)"
LOG_OUT="$DATA_ABS/bridge.stdout.log"
LOG_ERR="$DATA_ABS/bridge.stderr.log"

# ── Write auto-update wrapper ─────────────────────────────────
cat > "$DATA_ABS/start.sh" << HEADER_EOF
#!/bin/bash
BBCTL_DIR="$BBCTL_DIR"
BBCTL_BRANCH="$BBCTL_BRANCH"
BINARY="$BINARY"
CONFIG="$CONFIG_ABS"
HEADER_EOF
cat >> "$DATA_ABS/start.sh" << 'BODY_EOF'
BBCTL_REPO="${BBCTL_REPO:-https://github.com/lrhodin/imessage.git}"

# ANSI helpers
BOLD='\033[1m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
YELLOW='\033[0;33m'
DIM='\033[2m'
RESET='\033[0m'

ts()   { date '+%H:%M:%S'; }
ok()   { printf "${DIM}$(ts)${RESET}  ${GREEN}✓${RESET}  %s\n" "$*"; }
step() { printf "${DIM}$(ts)${RESET}  ${CYAN}▶${RESET}  %s\n" "$*"; }
warn() { printf "${DIM}$(ts)${RESET}  ${YELLOW}⚠${RESET}  %s\n" "$*"; }

printf "\n  ${BOLD}iMessage Bridge${RESET}\n\n"

# Bootstrap sparse clone if it doesn't exist yet
if [ ! -d "$BBCTL_DIR/.git" ] && command -v go >/dev/null 2>&1; then
    step "Setting up bbctl sparse checkout..."
    EXISTING_BBCTL=""
    [ -x "$BBCTL_DIR/bbctl" ] && EXISTING_BBCTL=$(mktemp)  && cp "$BBCTL_DIR/bbctl" "$EXISTING_BBCTL"
    rm -rf "$BBCTL_DIR"
    mkdir -p "$(dirname "$BBCTL_DIR")"
    git clone --filter=blob:none --no-checkout --quiet \
        --branch "$BBCTL_BRANCH" "$BBCTL_REPO" "$BBCTL_DIR"
    git -C "$BBCTL_DIR" sparse-checkout init --cone
    git -C "$BBCTL_DIR" sparse-checkout set cmd/bbctl
    git -C "$BBCTL_DIR" checkout --quiet "$BBCTL_BRANCH"
    (cd "$BBCTL_DIR" && go build -o bbctl ./cmd/bbctl/ 2>&1) | sed 's/^/  /'
    [ -n "$EXISTING_BBCTL" ] && rm -f "$EXISTING_BBCTL"
    ok "bbctl ready"
fi

if [ -d "$BBCTL_DIR/.git" ] && command -v go >/dev/null 2>&1; then
    git -C "$BBCTL_DIR" fetch origin --quiet 2>/dev/null || true
    LOCAL=$(git -C "$BBCTL_DIR" rev-parse --short HEAD 2>/dev/null || echo "unknown")
    REMOTE=$(git -C "$BBCTL_DIR" rev-parse --short "origin/$BBCTL_BRANCH" 2>/dev/null || echo "unknown")
    if [ "$LOCAL" != "$REMOTE" ] && [ "$LOCAL" != "unknown" ] && [ "$REMOTE" != "unknown" ]; then
        step "Updating bbctl  $LOCAL → $REMOTE"
        T0=$(date +%s)
        git -C "$BBCTL_DIR" reset --hard "origin/$BBCTL_BRANCH" --quiet
        step "Building bbctl..."
        (cd "$BBCTL_DIR" && go build -o bbctl ./cmd/bbctl/ 2>&1) | sed 's/^/  /'
        T1=$(date +%s)
        ok "bbctl updated  ($(( T1 - T0 ))s)"
    else
        ok "bbctl $LOCAL"
    fi
elif [ -d "$BBCTL_DIR/.git" ]; then
    warn "go not found — skipping bbctl update"
fi

# Fix permissions before starting — the config upgrader may have replaced
# the user's permissions with example.com defaults on a previous run.
# Detects: empty username (@:, @":), example.com defaults, wildcard relay.
if grep -q '"@:\|"@":\|@.*example\.com\|"\*":.*relay' "$CONFIG" 2>/dev/null; then
    BBCTL_BIN="$BBCTL_DIR/bbctl"
    if [ -x "$BBCTL_BIN" ]; then
        FIX_USER=$("$BBCTL_BIN" whoami 2>/dev/null | head -1 || true)
        if [ -n "$FIX_USER" ] && [ "$FIX_USER" != "null" ]; then
            FIX_MXID="@${FIX_USER}:beeper.com"
            sed -i '' '/permissions:/,/^[^ ]/{
                s/"@[^"]*": admin/"'"$FIX_MXID"'": admin/
                /@.*example\.com/d
                /"\*":.*relay/d
                /"@":/d
                /"@:/d
            }' "$CONFIG"
            ok "Fixed permissions: $FIX_MXID"
        fi
    fi
fi

step "Starting bridge..."
exec "$BINARY" -n -c "$CONFIG"
BODY_EOF
chmod +x "$DATA_ABS/start.sh"

mkdir -p "$(dirname "$PLIST")"
GUI_DOMAIN="gui/$(id -u)"
launchctl bootout "$GUI_DOMAIN/$BUNDLE_ID" 2>/dev/null || true
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
        <string>/bin/bash</string>
        <string>$DATA_ABS/start.sh</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$DATA_ABS</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>Crashed</key>
        <true/>
    </dict>
    <key>StandardOutPath</key>
    <string>$LOG_OUT</string>
    <key>StandardErrorPath</key>
    <string>$LOG_ERR</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin:/usr/local/go/bin:$HOME/go/bin</string>
        <key>CGO_CFLAGS</key>
        <string>-I/opt/homebrew/include</string>
        <key>CGO_LDFLAGS</key>
        <string>-L/opt/homebrew/lib</string>
    </dict>
</dict>
</plist>
PLIST_EOF

if ! launchctl bootstrap "$GUI_DOMAIN" "$PLIST" 2>/dev/null; then
    if ! launchctl load "$PLIST" 2>/dev/null; then
        echo ""
        echo "⚠  LaunchAgent failed to load. You can run the bridge manually:"
        echo "   $BINARY -c $CONFIG_ABS"
        echo ""
        echo "   This is a known issue on macOS 13 (Ventura). Try:"
        echo "   1. Remove and re-add the .app in Full Disk Access"
        echo "   2. Re-run: make install-beeper"
        echo ""
    fi
fi
echo "✓ Bridge started (LaunchAgent installed)"
echo ""

# ── Wait for bridge to connect ────────────────────────────────
DOMAIN=$(grep '^\s*domain:' "$CONFIG" | head -1 | awk '{print $2}' || true)
DOMAIN="${DOMAIN:-beeper.local}"

echo "Waiting for bridge to start..."
for i in $(seq 1 15); do
    if grep -q "Bridge started\|UNCONFIGURED\|Backfill queue starting" "$LOG_OUT" 2>/dev/null; then
        echo "✓ Bridge is running"
        echo ""
        echo "═══════════════════════════════════════════════"
        echo "  Setup Complete"
        echo "═══════════════════════════════════════════════"
        echo ""
        echo "  Logs:    tail -f $LOG_OUT"
        echo "  Stop:    launchctl bootout $GUI_DOMAIN/$BUNDLE_ID"
        echo "  Start:   launchctl bootstrap $GUI_DOMAIN $PLIST"
        echo "  Restart: launchctl kickstart -k $GUI_DOMAIN/$BUNDLE_ID"
        exit 0
    fi
    sleep 1
done

echo ""
echo "Bridge is starting up (check logs for status):"
echo "  tail -f $LOG_OUT"
echo ""
echo "Once running, DM @${BRIDGE_NAME}bot:$DOMAIN and send: login"
