package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"go.mau.fi/mautrix-imessage/imessage"
	"maunium.net/go/mautrix"
)

const historyRecoveryPlanEnv = "MAUTRIX_IMESSAGE_RECOVERY_PLAN"

type historyRecoveryConnector interface {
	GetHistoryRecoveryMessages(chatGUID string, since time.Time, backfillID string, messageGUIDs map[string]struct{}) ([]*imessage.Message, error)
}

type historyRecoveryPlan struct {
	Since        time.Time `json:"since"`
	Chats        []string  `json:"chats"`
	MessageGUIDs []string  `json:"message_guids"`
}

func parseHistoryRecoveryPlan(data []byte, now time.Time) (historyRecoveryPlan, error) {
	var plan historyRecoveryPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return plan, err
	}
	if plan.Since.IsZero() || !plan.Since.Before(now) {
		return plan, fmt.Errorf("since must be a past RFC3339 timestamp")
	}
	for _, guids := range [][]string{plan.Chats, plan.MessageGUIDs} {
		if len(guids) == 0 {
			return plan, fmt.Errorf("chats and message_guids must both be nonempty")
		}
		for _, guid := range guids {
			if guid == "" || strings.TrimSpace(guid) != guid {
				return plan, fmt.Errorf("invalid empty or whitespace-padded GUID")
			}
		}
	}
	return plan, nil
}

type startupRecovery struct {
	connector    historyRecoveryConnector
	since        time.Time
	pendingChats map[string]struct{}
	messages     map[string]struct{}
}

func (br *IMBridge) loadStartupRecoveryPlan() *startupRecovery {
	filename := os.Getenv(historyRecoveryPlanEnv)
	if filename == "" {
		return nil
	}
	connector, ok := br.IM.(historyRecoveryConnector)
	if !ok || !br.Config.Bridge.Backfill.Enable || !br.SpecVersions.Supports(mautrix.BeeperFeatureBatchSending) {
		br.ZLog.Warn().Msg("Startup history recovery requires the BlueBubbles connector and enabled Beeper batch backfill")
		return nil
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		br.ZLog.Error().Err(err).Msg("Cannot read startup history recovery plan")
		return nil
	}
	plan, err := parseHistoryRecoveryPlan(data, time.Now())
	if err != nil {
		br.ZLog.Error().Err(err).Msg("Invalid startup history recovery plan")
		return nil
	}
	recovery := &startupRecovery{
		connector:    connector,
		since:        plan.Since,
		pendingChats: make(map[string]struct{}, len(plan.Chats)),
		messages:     make(map[string]struct{}, len(plan.MessageGUIDs)),
	}
	for _, chat := range plan.Chats {
		recovery.pendingChats[chat] = struct{}{}
	}
	for _, guid := range plan.MessageGUIDs {
		recovery.messages[guid] = struct{}{}
	}
	return recovery
}

func (recovery *startupRecovery) runForPortal(portal *Portal) {
	if _, requested := recovery.pendingChats[portal.GUID]; !requested || portal.MXID == "" {
		return
	}
	delete(recovery.pendingChats, portal.GUID)

	portal.lockBackfill()
	defer portal.unlockBackfill()
	backfillID := fmt.Sprintf("bridge-repair-%s::%d", portal.Identifier.LocalID, time.Now().UnixMilli())
	portal.zlog.Info().Time("since", recovery.since).Msg("Checking selected chat for history recovery")
	messages, err := recovery.connector.GetHistoryRecoveryMessages(portal.GUID, recovery.since, backfillID, recovery.messages)
	if err != nil {
		portal.zlog.Error().Err(err).Msg("Failed to fetch messages for history recovery")
		return
	}
	if len(messages) == 0 {
		portal.zlog.Info().Msg("No selected messages available for history recovery")
		return
	}
	portal.sendBackfillWithRecovery(backfillID, messages, true, false, false, true)
}
