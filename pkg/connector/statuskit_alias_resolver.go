package connector

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

// StatusKit alias-resolver: persistent (channel_id ↔ alias) clustering and
// transitive portal resolution.
//
// Background: peer iOS fans every StatusKit reshare across all of the peer's
// registered handles, broadcasting the SAME channel id with a DIFFERENT
// sender handle each time. Some peers have aliases (older Apple ID emails,
// hidden mailto:s) that aren't registered with Apple's IDS service for
// iMessage or status-keysharing — IDS validate_targets returns 6001
// LookupFailed for those, and contacts often don't list them either. When
// presence arrives from such an alias, the standard resolution chain
// (learned-cache → contacts → IDS → mailto: portal) fails and the notice
// is dropped.
//
// The cluster store fixes this by persisting every observed
// (channel_id, sender_handle) pair across reshare events. Two senders
// observed on the same channel id are aliases of the same peer. When an
// unknown handle's presence arrives, we look up its channel_ids, list
// sibling handles in those clusters, and resolve through the persistent
// alias→portal map (or the live chain on the sibling). The first sibling
// that resolves gives us the unknown handle's portal too.
//
// Persistence layers:
//   - statuskit.alias_portal.<handle>      → portalID  (KV-backed cache)
//   - statuskit.channel_cluster.<channel>  → JSON [handles]
//   - statuskit.alias_channels.<handle>    → JSON [channel_ids]  (reverse)
//
// Data sources feeding the cluster:
//   - APNs reshares (live): OnReshareSender fires per alias, channel id
//     plumbed through from rust.
//   - StatusKit-CloudKit pull: every successfully-decoded
//     CD_ReceivedInvitation contributes (channel_id, sender_handle).
//
// This file is intentionally decoupled from the StatusKit-CloudKit pull
// implementation so the cluster store survives a future cutover to an
// upstream-rustpush native pull. The resolver consumes from durable
// callback/return-shape interfaces, not from any pull file's internals.

const (
	// statusKitAliasPortalKeyPrefix maps a peer handle to the network ID of
	// its DM portal. KV-backed mirror of statusKitPortalCache; persists
	// across bridge restarts so previously-resolved aliases remain O(1)
	// without re-paying IDS / contact-store lookups.
	statusKitAliasPortalKeyPrefix = "statuskit.alias_portal."

	// statusKitChannelClusterKeyPrefix maps a StatusKit channel id to a
	// JSON-encoded list of every peer handle observed publishing on it.
	// Aliases of the same peer share a channel id.
	statusKitChannelClusterKeyPrefix = "statuskit.channel_cluster."

	// statusKitAliasChannelsKeyPrefix is the reverse index — handle to
	// JSON-encoded list of channel ids the handle has been seen on. Used
	// during transitive resolution to find sibling aliases.
	statusKitAliasChannelsKeyPrefix = "statuskit.alias_channels."
)

// recordReshareObservation persists a (channel_id, sender_handle) pair
// into both the cluster forward map and the alias→channels reverse map.
// Idempotent — duplicate observations are no-ops.
//
// Called from OnReshareSender (live APNs reshares) and from the
// StatusKit-CloudKit pull (offline reshares recovered from iCloud).
func (c *IMClient) recordReshareObservation(ctx context.Context, log zerolog.Logger, channelID, senderHandle string) {
	if channelID == "" || senderHandle == "" {
		return
	}

	clusterKey := database.Key(statusKitChannelClusterKeyPrefix + channelID)
	cluster := decodeAliasList(c.Main.Bridge.DB.KV.Get(ctx, clusterKey))
	if !containsString(cluster, senderHandle) {
		cluster = append(cluster, senderHandle)
		if encoded, err := json.Marshal(cluster); err == nil {
			c.Main.Bridge.DB.KV.Set(ctx, clusterKey, string(encoded))
			log.Debug().
				Str("channel_id", channelID).
				Str("alias", senderHandle).
				Int("cluster_size", len(cluster)).
				Msg("StatusKit alias-resolver: cluster observation added")
		}
	}

	revKey := database.Key(statusKitAliasChannelsKeyPrefix + senderHandle)
	channels := decodeAliasList(c.Main.Bridge.DB.KV.Get(ctx, revKey))
	if !containsString(channels, channelID) {
		channels = append(channels, channelID)
		if encoded, err := json.Marshal(channels); err == nil {
			c.Main.Bridge.DB.KV.Set(ctx, revKey, string(encoded))
		}
	}
}

// resolveViaCluster runs the transitive lookup. For an unknown handle X,
// finds all channel_ids it has been observed on, then for each
// channel_id, examines sibling handles. For each sibling, checks the
// persistent alias→portal map first (cheap), and falls back to the
// live resolution chain only if needed. Returns the first portal that
// resolves, persists the X→portal mapping for future O(1) lookups.
func (c *IMClient) resolveViaCluster(ctx context.Context, log zerolog.Logger, unknown string) *bridgev2.Portal {
	if unknown == "" {
		return nil
	}
	channels := decodeAliasList(c.Main.Bridge.DB.KV.Get(ctx, database.Key(statusKitAliasChannelsKeyPrefix+unknown)))
	if len(channels) == 0 {
		return nil
	}

	visited := map[string]struct{}{unknown: {}}
	for _, channelID := range channels {
		cluster := decodeAliasList(c.Main.Bridge.DB.KV.Get(ctx, database.Key(statusKitChannelClusterKeyPrefix+channelID)))
		for _, sibling := range cluster {
			if _, dup := visited[sibling]; dup {
				continue
			}
			visited[sibling] = struct{}{}

			if portal := c.lookupAliasPortal(ctx, sibling); portal != nil {
				log.Info().
					Str("unknown", unknown).
					Str("via_alias", sibling).
					Str("channel_id", channelID).
					Str("portal_id", string(portal.ID)).
					Msg("StatusKit alias-resolver: resolved via persisted sibling")
				c.rememberAliasPortal(ctx, unknown, portal.ID)
				return portal
			}

			if portal := c.resolveSiblingHandleLive(ctx, log, sibling); portal != nil {
				log.Info().
					Str("unknown", unknown).
					Str("via_alias", sibling).
					Str("channel_id", channelID).
					Str("portal_id", string(portal.ID)).
					Msg("StatusKit alias-resolver: resolved via live sibling chain")
				c.rememberAliasPortal(ctx, sibling, portal.ID)
				c.rememberAliasPortal(ctx, unknown, portal.ID)
				return portal
			}
		}
	}
	return nil
}

// lookupAliasPortal checks the persistent alias→portal store for a known
// mapping and returns the active portal if one exists. In-memory check
// happens via statusKitPortalCache; KV check is the persistent fallback.
func (c *IMClient) lookupAliasPortal(ctx context.Context, alias string) *bridgev2.Portal {
	if cached, ok := c.statusKitPortalCache.Load(alias); ok {
		if p := c.findPortalByID(ctx, cached.(networkid.PortalID)); p != nil {
			return p
		}
	}
	raw := c.Main.Bridge.DB.KV.Get(ctx, database.Key(statusKitAliasPortalKeyPrefix+alias))
	if raw == "" {
		return nil
	}
	pid := networkid.PortalID(raw)
	c.statusKitPortalCache.Store(alias, pid)
	return c.findPortalByID(ctx, pid)
}

// resolveSiblingHandleLive runs the cheap, non-IDS portion of the
// standard resolution chain on a sibling handle. IDS is skipped for two
// reasons: (a) the unknown handle that triggered cluster lookup almost
// certainly already failed IDS in the caller, and the sibling is in the
// same cluster so it's likely the same kind of alias; (b) IDS cost is
// per-call and we may iterate several siblings — keeping this tier
// cheap matters.
func (c *IMClient) resolveSiblingHandleLive(ctx context.Context, log zerolog.Logger, sibling string) *bridgev2.Portal {
	normalized := normalizeIdentifierForPortalID(sibling)

	// Address book: a sibling listed as a contact resolves through the
	// contact store's portal IDs.
	if contact := c.lookupContact(sibling); contact != nil {
		for _, altID := range contactPortalIDs(contact) {
			if !strings.HasPrefix(altID, "tel:") && !strings.HasPrefix(altID, "mailto:") {
				continue
			}
			if p := c.findPortalByID(ctx, networkid.PortalID(altID)); p != nil {
				return p
			}
		}
	}

	// Direct portal lookup (handles tel: senders that are themselves the
	// portal id, and mailto: senders that have a mailto: portal).
	if !strings.HasPrefix(normalized, "mailto:") {
		portalID := c.resolveContactPortalID(normalized)
		portalID = c.resolveExistingDMPortalID(string(portalID))
		if p := c.findPortalByID(ctx, portalID); p != nil {
			return p
		}
	} else {
		if p := c.findPortalByID(ctx, networkid.PortalID(normalized)); p != nil {
			return p
		}
	}
	return nil
}

// findPortalByID is a small helper that returns the active portal for
// the user's login or nil if not found / not joined.
func (c *IMClient) findPortalByID(ctx context.Context, id networkid.PortalID) *bridgev2.Portal {
	p, err := c.Main.Bridge.GetExistingPortalByKey(ctx, networkid.PortalKey{
		ID:       id,
		Receiver: c.UserLogin.ID,
	})
	if err != nil || p == nil || p.MXID == "" {
		return nil
	}
	return p
}

// rememberAliasPortal writes alias→portal into both the in-memory cache
// and the persistent KV mirror. Use this everywhere the bridge resolves
// a StatusKit alias so future lookups are O(1) and survive restarts.
func (c *IMClient) rememberAliasPortal(ctx context.Context, alias string, portalID networkid.PortalID) {
	if alias == "" || portalID == "" {
		return
	}
	c.statusKitPortalCache.Store(alias, portalID)
	c.Main.Bridge.DB.KV.Set(ctx, database.Key(statusKitAliasPortalKeyPrefix+alias), string(portalID))
}

// hydrateAliasPortalCacheFromKV pre-loads the in-memory
// statusKitPortalCache from the persistent KV store so previously
// resolved aliases are usable on the very first presence update after a
// restart. Reads are bulk against the KV table.
func (c *IMClient) hydrateAliasPortalCacheFromKV(ctx context.Context, log zerolog.Logger) {
	rows, err := c.Main.Bridge.DB.RawDB.QueryContext(ctx,
		"SELECT key, value FROM kv WHERE key LIKE $1",
		statusKitAliasPortalKeyPrefix+"%")
	if err != nil {
		log.Warn().Err(err).Msg("StatusKit alias-resolver: KV hydration query failed")
		return
	}
	defer rows.Close()
	loaded := 0
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			continue
		}
		alias := strings.TrimPrefix(key, statusKitAliasPortalKeyPrefix)
		c.statusKitPortalCache.Store(alias, networkid.PortalID(value))
		loaded++
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("StatusKit alias-resolver: KV hydration row iteration error")
	}
	log.Info().Int("loaded", loaded).Msg("StatusKit alias-resolver: hydrated alias→portal cache from KV")
}

// prewarmAliasPortalCache scans every ghost in the bridge DB and seeds
// (handle → portal) mappings via the cheap resolution path (address
// book + direct portal id). Catches contacts already known to the
// bridge without any IDS round-trips. Run once during connect after
// hydrate so we don't redo the work for aliases already in KV.
func (c *IMClient) prewarmAliasPortalCache(ctx context.Context, log zerolog.Logger) {
	rows, err := c.Main.Bridge.DB.RawDB.QueryContext(ctx, "SELECT id FROM ghost WHERE bridge_id=$1", c.Main.Bridge.ID)
	if err != nil {
		log.Warn().Err(err).Msg("StatusKit alias-resolver: pre-warm ghost scan failed")
		return
	}
	defer rows.Close()
	primed := 0
	for rows.Next() {
		var ghostID string
		if err := rows.Scan(&ghostID); err != nil {
			continue
		}
		if !strings.HasPrefix(ghostID, "mailto:") && !strings.HasPrefix(ghostID, "tel:") {
			continue
		}
		if _, ok := c.statusKitPortalCache.Load(ghostID); ok {
			continue
		}
		if portal := c.resolveSiblingHandleLive(ctx, log, ghostID); portal != nil {
			c.rememberAliasPortal(ctx, ghostID, portal.ID)
			primed++
		}
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("StatusKit alias-resolver: pre-warm row iteration error")
	}
	log.Info().Int("primed", primed).Msg("StatusKit alias-resolver: pre-warm complete")
}

func decodeAliasList(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}
