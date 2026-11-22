package bluebubbles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	readReceiptQueueSize = 32
	readReceiptTimeout   = 5 * time.Second
)

type queuedReadReceipt struct {
	chatID   string
	deadline time.Time
}

// SendReadReceipt queues a best-effort read receipt before returning to Matrix
// event processing. BlueBubbles marks the whole chat as read.
func (bb *blueBubbles) SendReadReceipt(chatID, _ string) error {
	if !bb.usingPrivateAPI {
		return errors.ErrUnsupported
	}
	if err := bb.readReceiptContext.Err(); err != nil {
		return err
	}

	select {
	case bb.readReceipts <- queuedReadReceipt{chatID: chatID, deadline: time.Now().Add(readReceiptTimeout)}:
		return nil
	default:
		return errors.New("BlueBubbles read receipt queue is full")
	}
}

func (bb *blueBubbles) runReadReceipts() {
	for {
		select {
		case <-bb.readReceiptContext.Done():
			return
		case receipt := <-bb.readReceipts:
			if !time.Now().Before(receipt.deadline) {
				continue
			}
			if err := bb.markChatRead(receipt); err != nil && bb.readReceiptContext.Err() == nil {
				bb.log.Warn().Err(err).Msg("Could not mark BlueBubbles chat as read")
			}
		}
	}
}

func (bb *blueBubbles) markChatRead(receipt queuedReadReceipt) error {
	ctx, cancel := context.WithDeadline(bb.readReceiptContext, receipt.deadline)
	defer cancel()
	path := fmt.Sprintf("/api/v1/chat/%s/read", receipt.chatID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, bb.apiURL(path, nil), nil)
	if err != nil {
		return errors.New("invalid URL for BlueBubbles read receipt")
	}
	req.Header.Set("Content-Type", "application/json")

	// Do not hold bbRequestLock: a slow receipt must not block a send or an
	// inbound event's metadata requests.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// The URL includes the BlueBubbles password as a query parameter.
		var requestErr *url.Error
		if errors.As(err, &requestErr) {
			return requestErr.Err
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("BlueBubbles read receipt returned HTTP %d", resp.StatusCode)
	}

	var result ReadReceiptResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16*1024)).Decode(&result); err != nil {
		return err
	}
	if result.Status != http.StatusOK {
		return fmt.Errorf("BlueBubbles read receipt returned status %d", result.Status)
	}
	return nil
}
