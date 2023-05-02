// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2023 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"crypto/hmac"
	cryptoRand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	"maunium.net/go/mautrix/crypto/utils"

	"go.mau.fi/mautrix-imessage/database"
	"go.mau.fi/mautrix-imessage/imessage"
)

func encryptTestPayload(keyStr string, payload map[string]any) (map[string]any, error) {
	key, err := base64.RawStdEncoding.DecodeString(keyStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode key: %w", err)
	}
	aesKey, hmacKey := utils.DeriveKeysSHA256(key, "meow")
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}
	iv := make([]byte, utils.AESCTRIVLength)
	_, err = cryptoRand.Read(iv)
	if err != nil {
		return nil, fmt.Errorf("failed to generate iv: %w", err)
	}
	data = utils.XorA256CTR(data, aesKey, *(*[utils.AESCTRIVLength]byte)(iv))

	h := hmac.New(sha256.New, hmacKey[:])
	h.Write(data)
	h.Write(iv)
	return map[string]any{
		"ciphertext": base64.RawStdEncoding.EncodeToString(data),
		"checksum":   base64.RawStdEncoding.EncodeToString(h.Sum(nil)),
		"iv":         base64.RawStdEncoding.EncodeToString(iv),
	}, nil
}

func decryptTestPayload(keyStr string, encrypted map[string]any) (map[string]any, error) {
	key, err := base64.RawStdEncoding.DecodeString(keyStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode key: %w", err)
	}
	var ciphertext, checksum, iv []byte
	getKey := func(key string) ([]byte, error) {
		val, ok := encrypted[key].(string)
		if !ok {
			return nil, fmt.Errorf("missing %s in encrypted payload", key)
		}
		decoded, err := base64.RawStdEncoding.DecodeString(val)
		if err != nil {
			return nil, fmt.Errorf("failed to decode %s: %w", key, err)
		}
		return decoded, nil
	}
	if ciphertext, err = getKey("ciphertext"); err != nil {
		return nil, err
	} else if checksum, err = getKey("checksum"); err != nil {
		return nil, err
	} else if iv, err = getKey("iv"); err != nil {
		return nil, err
	} else if len(iv) != utils.AESCTRIVLength {
		return nil, fmt.Errorf("incorrect iv length %d", len(iv))
	}
	aesKey, hmacKey := utils.DeriveKeysSHA256(key, "meow")
	h := hmac.New(sha256.New, hmacKey[:])
	h.Write(ciphertext)
	h.Write(iv)
	if !hmac.Equal(h.Sum(nil), checksum) {
		return nil, fmt.Errorf("mismatching hmac")
	}
	var payload map[string]any
	err = json.Unmarshal(utils.XorA256CTR(ciphertext, aesKey, *(*[utils.AESCTRIVLength]byte)(iv)), &payload)
	return payload, err
}

const startupTestKey = "com.beeper.startup_test"
const startupTestIDKey = "com.beeper.startup_test_id"
const startupTestResponseKey = "com.beeper.startup_test_response"

const hackyTestSegmentEvent = "iMC startup test"
const hackyTestLogAction = "hacky startup test"

func trackStartupTestError(erroredAt, randomID string) {
	Segment.Track(hackyTestSegmentEvent, map[string]any{
		"event":     "fail",
		"error":     erroredAt,
		"random_id": randomID,
	})
}

func (br *IMBridge) hackyTestLoop() {
	log := br.ZLog.With().
		Str("identifier", br.Config.HackyStartupTest.Identifier).
		Str("action", "hacky periodic test").
		Logger()
	for {
		time.Sleep(time.Duration(br.Config.HackyStartupTest.PeriodicResolve) * time.Second)
		log.Debug().Msg("Sending hacky periodic test")
		resp, err := br.WebsocketHandler.StartChat(StartDMRequest{
			Identifier: br.Config.HackyStartupTest.Identifier,
		})
		if err != nil {
			log.Error().Err(err).Msg("Failed to resolve identifier")
		} else {
			log.Info().Interface("response", resp).Msg("Successfully resolved identifier")
		}

	}
}

func (br *IMBridge) hackyStartupTests(sleep, forceSend bool) {
	if sleep {
		time.Sleep(time.Duration(rand.Intn(120)+60) * time.Second)
	}
	br.DB.KV.Set(database.KVBridgeWasConnected, "true")
	randomID := br.Bot.TxnID()
	log := br.ZLog.With().
		Str("identifier", br.Config.HackyStartupTest.Identifier).
		Str("random_id", randomID).
		Str("action", hackyTestLogAction).
		Logger()
	resp, err := br.WebsocketHandler.StartChat(StartDMRequest{
		Identifier:    br.Config.HackyStartupTest.Identifier,
		ActuallyStart: br.Config.HackyStartupTest.Message != "",
	})
	if err != nil {
		log.Error().Err(err).Msg("Failed to resolve identifier")
		trackStartupTestError("start chat", randomID)
		return
	}
	log.Info().Interface("response", resp).Msg("Successfully resolved identifier")
	if br.Config.HackyStartupTest.Message == "" || (!br.Config.HackyStartupTest.SendOnStartup && !forceSend) {
		return
	}
	portal := br.GetPortalByGUID(resp.GUID)

	payload, err := encryptTestPayload(br.Config.HackyStartupTest.Key, map[string]any{
		"segment_user_id": Segment.userID,
		"user_id":         br.user.MXID.String(),
		"random_id":       randomID,
	})
	if err != nil {
		trackStartupTestError("encrypt payload", randomID)
		log.Error().Err(err).Msg("Failed to encrypt ping payload")
		return
	}
	metadata := map[string]any{
		startupTestKey:   payload,
		startupTestIDKey: randomID,
	}
	sendResp, err := br.IM.SendMessage(portal.getTargetGUID("text message", "startup test", ""), br.Config.HackyStartupTest.Message, "", 0, nil, metadata)
	if err != nil {
		log.Error().Err(err).Msg("Failed to send message")
		trackStartupTestError("send message", randomID)
		return
	}
	br.pendingHackyTestGUID = sendResp.GUID
	br.pendingHackyTestRandomID = randomID
	log.Info().Str("msg_guid", sendResp.GUID).Msg("Successfully sent message")
	Segment.Track(hackyTestSegmentEvent, map[string]any{
		"event":     "message sent",
		"random_id": randomID,
		"msg_guid":  sendResp.GUID,
	})
	go func() {
		time.Sleep(30 * time.Second)
		if !br.hackyTestSuccess {
			log.Info().Msg("Hacky test success flag not set, sending timeout event")
			Segment.Track(hackyTestSegmentEvent, map[string]any{
				"event":     "timeout",
				"random_id": randomID,
				"msg_guid":  sendResp.GUID,
			})
		}
	}()
}

func (br *IMBridge) trackStartupTestPingStatus(msgStatus *imessage.SendMessageStatus) {
	br.ZLog.Info().
		Interface("status", msgStatus).
		Str("random_id", br.pendingHackyTestRandomID).
		Str("action", hackyTestLogAction).
		Msg("Received message status for hacky test")
	meta := map[string]any{
		"msg_guid":  msgStatus.GUID,
		"random_id": br.pendingHackyTestRandomID,
	}
	switch msgStatus.Status {
	case "delivered":
		meta["event"] = "ping delivered"
	case "sent":
		meta["event"] = "ping sent"
	case "failed":
		meta["event"] = "ping failed"
		meta["error"] = msgStatus.Message
	default:
		return
	}
	Segment.Track(hackyTestSegmentEvent, meta)
}

func (br *IMBridge) receiveStartupTestPing(msg *imessage.Message) {
	unencryptedID, ok := msg.Metadata[startupTestIDKey].(string)
	if !ok {
		return
	}
	encryptedPayload, ok := msg.Metadata[startupTestKey].(map[string]any)
	if !ok {
		return
	}
	log := br.ZLog.With().
		Str("action", hackyTestLogAction).
		Str("random_id", unencryptedID).
		Str("msg_guid", msg.GUID).
		Str("chat_guid", msg.ChatGUID).
		Str("sender_guid", msg.Sender.String()).
		Logger()
	log.Info().Msg("Received startup test ping")
	payload, err := decryptTestPayload(br.Config.HackyStartupTest.Key, encryptedPayload)
	if err != nil {
		Segment.Track(hackyTestSegmentEvent, map[string]any{
			"event":     "fail",
			"error":     "ping decrypt",
			"random_id": unencryptedID,
			"msg_guid":  msg.GUID,
		})
		return
	}
	userID, _ := payload["user_id"].(string)
	segmentUserID, _ := payload["segment_user_id"].(string)
	log = log.With().
		Str("matrix_user_id", userID).
		Str("segment_user_id", segmentUserID).
		Logger()
	log.Info().Msg("Decrypted startup test ping")
	randomID, ok := payload["random_id"].(string)
	if randomID != unencryptedID {
		log.Warn().Str("encrypted_random_id", randomID).Msg("Mismatching random ID in encrypted payload")
		Segment.TrackUser(hackyTestSegmentEvent, segmentUserID, map[string]any{
			"event":     "fail",
			"error":     "mismatching random ID",
			"random_id": randomID,
			"msg_guid":  msg.GUID,
		})
		return
	}
	Segment.TrackUser(hackyTestSegmentEvent, segmentUserID, map[string]any{
		"event":     "ping received",
		"random_id": unencryptedID,
		"msg_guid":  msg.GUID,
	})
	time.Sleep(2 * time.Second)
	resp, err := br.IM.SendMessage(msg.ChatGUID, br.Config.HackyStartupTest.ResponseMessage, msg.GUID, 0, nil, map[string]any{
		startupTestResponseKey: map[string]any{
			"random_id": randomID,
		},
	})
	if err != nil {
		log.Error().Err(err).Msg("Failed to send pong")
		Segment.TrackUser(hackyTestSegmentEvent, segmentUserID, map[string]any{
			"event":     "fail",
			"error":     "pong send",
			"random_id": unencryptedID,
			"msg_guid":  msg.GUID,
		})
		return
	}
	log.Info().Str("pong_msg_guid", resp.GUID).Msg("Sent pong")
	Segment.TrackUser(hackyTestSegmentEvent, segmentUserID, map[string]any{
		"event":     "pong sent",
		"random_id": unencryptedID,
		"msg_guid":  msg.GUID,
		"pong_guid": resp.GUID,
	})
}

func (br *IMBridge) receiveStartupTestPong(resp map[string]any, msg *imessage.Message) {
	randomID, _ := resp["random_id"].(string)
	br.ZLog.Info().
		Str("action", hackyTestLogAction).
		Str("random_id", randomID).
		Str("msg_guid", msg.GUID).
		Str("reply_to_guid", msg.ReplyToGUID).
		Msg("Startup test successful")
	Segment.Track(hackyTestSegmentEvent, map[string]any{
		"event":     "success",
		"random_id": randomID,
		"pong_guid": msg.GUID,
		"msg_guid":  msg.ReplyToGUID,
	})
	if br.pendingHackyTestRandomID == randomID {
		br.hackyTestSuccess = true
		br.pendingHackyTestGUID = ""
		br.pendingHackyTestRandomID = ""
	} else {
		br.ZLog.Info().
			Str("pending_random_id", br.pendingHackyTestRandomID).
			Str("pending_msg_guid", br.pendingHackyTestGUID).
			Msg("Pending test ID isn't same as received ID")
	}
}
