package upgrades

import (
	"database/sql"
)

const createPortalTable = `CREATE TABLE portal (
	guid       TEXT    PRIMARY KEY,
	mxid       TEXT    UNIQUE,
	name       TEXT    NOT NULL,
	avatar     TEXT    NOT NULL,
	avatar_url TEXT    NOT NULL,
	encrypted  BOOLEAN NOT NULL DEFAULT 0
)`

const createPuppetTable = `CREATE TABLE puppet (
	id           TEXT PRIMARY KEY,
	displayname  TEXT NOT NULL,
	avatar       TEXT NOT NULL,
	avatar_url   TEXT NOT NULL
)`

const createUserTable = `CREATE TABLE "user" (
	mxid            TEXT PRIMARY KEY,
	management_room TEXT NOT NULL,
	access_token    TEXT NOT NULL,
	next_batch      TEXT NOT NULL
)`

const createMessageTable = `CREATE TABLE message (
	chat_guid     TEXT REFERENCES portal(guid) ON DELETE CASCADE,
	guid          TEXT,
	mxid          TEXT NOT NULL UNIQUE,
	sender_guid   TEXT NOT NULL,
	timestamp     BIGINT NOT NULL,
	PRIMARY KEY (chat_guid, guid)
)`

const createTapbackTable = `CREATE TABLE tapback (
	chat_guid    TEXT,
	message_guid TEXT,
	sender_guid  TEXT,
    type         INTEGER NOT NULL,
	mxid         TEXT NOT NULL UNIQUE,
	PRIMARY KEY (chat_guid, message_guid, sender_guid),
	FOREIGN KEY (chat_guid, message_guid) REFERENCES message(chat_guid, guid) ON DELETE CASCADE
)`

const createUserProfileTable = `CREATE TABLE mx_user_profile (
	room_id TEXT,
	user_id TEXT,
	membership  TEXT NOT NULL,
	displayname TEXT NOT NULL DEFAULT '',
	avatar_url  TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (room_id, user_id)
)`

const createRoomStateTable = `CREATE TABLE mx_room_state (
	room_id      TEXT PRIMARY KEY,
	power_levels TEXT NOT NULL
)`

const createRegistrationsTable = `CREATE TABLE mx_registrations (
	user_id TEXT PRIMARY KEY
)`

const createCryptoAccountTable = `CREATE TABLE IF NOT EXISTS crypto_account (
	account_id VARCHAR(255) PRIMARY KEY,
	device_id  VARCHAR(255) NOT NULL,
	shared     BOOLEAN      NOT NULL,
	sync_token TEXT         NOT NULL,
	account    BLOB         NOT NULL
)`

const createCryptoMessageIndexTable = `CREATE TABLE IF NOT EXISTS crypto_message_index (
	sender_key CHAR(43),
	session_id CHAR(43),
	"index"    INTEGER,
	event_id   VARCHAR(255) NOT NULL,
	timestamp  BIGINT       NOT NULL,
	PRIMARY KEY (sender_key, session_id, "index")
)`

const createCryptoTrackedUserTable = `CREATE TABLE IF NOT EXISTS crypto_tracked_user (
	user_id VARCHAR(255) PRIMARY KEY
)`

const createCryptoDeviceTable = `CREATE TABLE IF NOT EXISTS crypto_device (
	user_id      VARCHAR(255),
	device_id    VARCHAR(255),
	identity_key CHAR(43)      NOT NULL,
	signing_key  CHAR(43)      NOT NULL,
	trust        SMALLINT      NOT NULL,
	deleted      BOOLEAN       NOT NULL,
	name         VARCHAR(255)  NOT NULL,
	PRIMARY KEY (user_id, device_id)
)`

const createCryptoOlmSessionTable = `CREATE TABLE IF NOT EXISTS crypto_olm_session (
	account_id   VARCHAR(255),
	session_id   CHAR(43),
	sender_key   CHAR(43)  NOT NULL,
	session      BLOB      NOT NULL,
	created_at   timestamp NOT NULL,
	last_used    timestamp NOT NULL,
	PRIMARY KEY (account_id, session_id)
)`

const createCryptoMegolmInboundSessionTable = `CREATE TABLE IF NOT EXISTS crypto_megolm_inbound_session (
	account_id   VARCHAR(255),
	session_id   CHAR(43),
	sender_key   CHAR(43) NOT NULL,
	signing_key  CHAR(43),
	room_id      VARCHAR(255) NOT NULL,
	session           BLOB,
	forwarding_chains BLOB,
	withheld_code     VARCHAR(255),
	withheld_reason   TEXT,
	PRIMARY KEY (account_id, session_id)
)`

const createCryptoMegolmOutboundSessionTable = `CREATE TABLE IF NOT EXISTS crypto_megolm_outbound_session (
	account_id    VARCHAR(255),
	room_id       VARCHAR(255),
	session_id    CHAR(43)     NOT NULL UNIQUE,
	session       BLOB         NOT NULL,
	shared        BOOLEAN      NOT NULL,
	max_messages  INTEGER      NOT NULL,
	message_count INTEGER      NOT NULL,
	max_age       BIGINT       NOT NULL,
	created_at    timestamp    NOT NULL,
	last_used     timestamp    NOT NULL,
	PRIMARY KEY (account_id, room_id)
)`

const createCryptoCrossSigningKeysTable = `CREATE TABLE IF NOT EXISTS crypto_cross_signing_keys (
	user_id VARCHAR(255) NOT NULL,
	usage   VARCHAR(20)  NOT NULL,
	key     CHAR(43)     NOT NULL,
	PRIMARY KEY (user_id, usage)
)`

const createCryptoCrossSigningSignaturesTable = `CREATE TABLE IF NOT EXISTS crypto_cross_signing_signatures (
	signed_user_id VARCHAR(255) NOT NULL,
	signed_key     VARCHAR(255) NOT NULL,
	signer_user_id VARCHAR(255) NOT NULL,
	signer_key     VARCHAR(255) NOT NULL,
	signature      CHAR(88)     NOT NULL,
	PRIMARY KEY (signed_user_id, signed_key, signer_user_id, signer_key)
)`

func init() {
	upgrades[0] = upgrade{"Initial schema", func(tx *sql.Tx, ctx context) error {
		_, err := tx.Exec(createPortalTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createPuppetTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createUserTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createMessageTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createTapbackTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createUserProfileTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createRoomStateTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createRegistrationsTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createCryptoAccountTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createCryptoMessageIndexTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createCryptoTrackedUserTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createCryptoDeviceTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createCryptoOlmSessionTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createCryptoMegolmInboundSessionTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createCryptoMegolmOutboundSessionTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createCryptoCrossSigningKeysTable)
		if err != nil {
			return err
		}
		_, err = tx.Exec(createCryptoCrossSigningSignaturesTable)
		if err != nil {
			return err
		}

		return nil
	}}
}
