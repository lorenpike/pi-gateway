package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

const (
	httpDeliveryQueueSize = 16
	maxHTTPMediaBytes     = int64(32 << 20)
)

var errNoHTTPReceiver = errors.New("http channel has no active receiver")

type httpDelivery struct {
	Event string
	Data  any
}

type httpTextDelivery struct {
	Text string `json:"text"`
}

type httpAttachmentDelivery struct {
	FileName string `json:"fileName"`
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
	Caption  string `json:"caption,omitempty"`
}

type httpDeliverySubscriber struct {
	deliveries chan httpDelivery
}

type httpDeliveryHub struct {
	mu          sync.Mutex
	subscribers map[string]map[*httpDeliverySubscriber]struct{}
}

func newHTTPDeliveryHub() *httpDeliveryHub {
	return &httpDeliveryHub{subscribers: make(map[string]map[*httpDeliverySubscriber]struct{})}
}

func (h *httpDeliveryHub) subscribe(channel string) (*httpDeliverySubscriber, func()) {
	sub := &httpDeliverySubscriber{deliveries: make(chan httpDelivery, httpDeliveryQueueSize)}
	h.mu.Lock()
	if h.subscribers[channel] == nil {
		h.subscribers[channel] = make(map[*httpDeliverySubscriber]struct{})
	}
	h.subscribers[channel][sub] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	return sub, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subscribers[channel], sub)
			if len(h.subscribers[channel]) == 0 {
				delete(h.subscribers, channel)
			}
			h.mu.Unlock()
		})
	}
}

// publish queues one atomic direct-send batch for every active HTTP subscriber.
// Holding the lock while checking and enqueueing prevents a send from reporting
// success after its final receiver has detached.
func (h *httpDeliveryHub) publish(channel string, deliveries []httpDelivery) error {
	if len(deliveries) == 0 {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	subs := h.subscribers[channel]
	if len(subs) == 0 {
		return errNoHTTPReceiver
	}
	for sub := range subs {
		if cap(sub.deliveries)-len(sub.deliveries) < len(deliveries) {
			return errors.New("http channel receiver is not accepting deliveries")
		}
	}
	for sub := range subs {
		for _, delivery := range deliveries {
			sub.deliveries <- delivery
		}
	}
	return nil
}

type httpSendAdapter struct {
	hub *httpDeliveryHub
}

func (a httpSendAdapter) Send(ctx context.Context, req SendRequest) (SendResult, error) {
	if err := ctx.Err(); err != nil {
		return SendResult{}, err
	}
	var deliveries []httpDelivery
	var sent []SentItem
	if req.Text != "" {
		deliveries = append(deliveries, httpDelivery{Event: "message", Data: httpTextDelivery{Text: req.Text}})
		sent = append(sent, SentItem{Type: "text", Text: req.Text})
	}
	if req.MediaPath != "" {
		attachment, err := encodeHTTPAttachment(req.MediaPath, req.Caption)
		if err != nil {
			return SendResult{}, err
		}
		deliveries = append(deliveries, httpDelivery{Event: "attachment", Data: attachment})
		sent = append(sent, SentItem{Type: "media", Path: req.MediaPath})
	}
	if err := a.hub.publish(req.Channel, deliveries); err != nil {
		return SendResult{}, err
	}
	return SendResult{Sent: sent}, nil
}

func encodeHTTPAttachment(path, caption string) (httpAttachmentDelivery, error) {
	file, err := os.Open(path)
	if err != nil {
		return httpAttachmentDelivery{}, fmt.Errorf("open HTTP attachment: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxHTTPMediaBytes+1))
	if err != nil {
		return httpAttachmentDelivery{}, fmt.Errorf("read HTTP attachment: %w", err)
	}
	if int64(len(data)) > maxHTTPMediaBytes {
		return httpAttachmentDelivery{}, fmt.Errorf("HTTP attachment exceeds %d-byte limit", maxHTTPMediaBytes)
	}
	mimeType := mime.TypeByExtension(filepath.Ext(path))
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	return httpAttachmentDelivery{
		FileName: filepath.Base(path),
		MimeType: mimeType,
		Data:     base64.StdEncoding.EncodeToString(data),
		Caption:  caption,
	}, nil
}
