package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jagadeesh/grainlify/backend/internal/config"
)

// Telegram sink for support requests.
//
// # The group is public
//
// @Grainlify is readable without joining, so what reaches a topic is
// published. Topic posts carry the message, the page URL and the support ID -
// never a GitHub login, an email, an internal user id, or the reporter's IP.
// The support ID is the link back to the full row.
//
// # KYC does not post publicly at all
//
// KYC requests go to the admin's direct message and nowhere else. An earlier
// design posted a stub ("🪪 KYC request #1234 - replied privately") alongside
// the DM; it was dropped because it disclosed that somebody asked a
// verification question at a timestamp - correlatable against anything else in
// the group at that moment, which identifies the person the redaction existed
// to protect - while telling nobody anything they could act on. The reporter is
// already acknowledged in the widget and the admin already has the DM.
//
// A failed admin DM is therefore a failed KYC delivery, full stop, and is
// logged at error level naming that specifically. It is recorded in its own
// column so "the details reached nobody" can never be confused with a normal
// delivery.
//
// # Failures never reach the reporter
//
// Everything here runs after the row is durable. A 403, a dead topic, a rate
// limit - none of them change the reporter's 200. Their report succeeded; our
// delivery is our problem.
type telegramSupportSink struct {
	token       string
	chatID      string
	adminUserID string
	topics      map[string]string
	httpClient  *http.Client
	// Injectable so tests can point at a local server. Never configurable from
	// the environment - the API host is not a deployment concern and a
	// settable one is a way to exfiltrate the bot token.
	baseURL string
}

func newTelegramSupportSink(cfg telegramSinkConfig) *telegramSupportSink {
	return &telegramSupportSink{
		token:       strings.TrimSpace(cfg.BotToken),
		chatID:      strings.TrimSpace(cfg.ChatID),
		adminUserID: strings.TrimSpace(cfg.AdminUserID),
		topics: map[string]string{
			"bug":   strings.TrimSpace(cfg.TopicBugs),
			"kyc":   strings.TrimSpace(cfg.TopicKYC),
			"idea":  strings.TrimSpace(cfg.TopicIdeas),
			"help":  strings.TrimSpace(cfg.TopicHelp),
			"other": strings.TrimSpace(cfg.TopicOther),
		},
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    "https://api.telegram.org",
	}
}

type telegramSinkConfig struct {
	BotToken, ChatID, AdminUserID                          string
	TopicBugs, TopicKYC, TopicIdeas, TopicHelp, TopicOther string
}

func (s *telegramSupportSink) Name() string { return "telegram" }

// Configured requires the token and chat id. Topic ids are checked per
// category at send time, so a single missing topic degrades to the General
// fallback rather than disabling the whole sink.
func (s *telegramSupportSink) Configured() bool {
	return s.token != "" && s.chatID != ""
}

// Handles takes every category. KYC is not excluded here the way it is from
// Discord - it is routed to the admin's DM instead of a topic, which is a
// different answer to the same question. Deliver decides which.
func (s *telegramSupportSink) Handles(category string) bool { return true }

// telegramResponse is the envelope every Bot API call returns.
type telegramResponse struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
	Parameters  *struct {
		// Set on 429. Parsed defensively - the field is documented as optional
		// and this code must not depend on it being present.
		RetryAfter int `json:"retry_after"`
		// Set when a group is upgraded to a supergroup and its id changes.
		// Logged prominently and NEVER auto-persisted: silently following a
		// chat id from an error response would mean the destination could
		// change without anybody deciding it had.
		MigrateToChatID int64 `json:"migrate_to_chat_id"`
	} `json:"parameters"`
	Result *struct {
		MessageID       int64 `json:"message_id"`
		MessageThreadID int64 `json:"message_thread_id"`
	} `json:"result"`
}

// sendMessage posts one message. threadID empty means the General thread.
func (s *telegramSupportSink) sendMessage(ctx context.Context, chatID, threadID, text string) (*telegramResponse, error) {
	form := url.Values{}
	form.Set("chat_id", chatID)
	form.Set("text", text)
	form.Set("disable_web_page_preview", "true")
	if threadID != "" {
		form.Set("message_thread_id", threadID)
	}

	endpoint := s.baseURL + "/bot" + s.token + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build telegram request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		// Never include the endpoint in an error: it carries the bot token.
		return nil, fmt.Errorf("telegram request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	var parsed telegramResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("telegram returned unparsable body (status %d)", resp.StatusCode)
	}
	if !parsed.OK {
		if parsed.Parameters != nil {
			if parsed.Parameters.RetryAfter > 0 {
				// 20 messages/minute is per CHAT, so all five topics share one
				// budget. Treated as "not delivered, try later" - the row stays
				// and is picked up by a replay - never as a lost report.
				slog.Warn("telegram: rate limited",
					"retry_after_seconds", parsed.Parameters.RetryAfter,
					"error_code", parsed.ErrorCode)
			}
			if parsed.Parameters.MigrateToChatID != 0 {
				slog.Error("telegram: CHAT ID HAS CHANGED - update TELEGRAM_CHAT_ID",
					"old_chat_id", chatID,
					"new_chat_id", parsed.Parameters.MigrateToChatID,
					"note", "not applied automatically; the destination must not move without a decision")
			}
		}
		return &parsed, fmt.Errorf("telegram error %d: %s", parsed.ErrorCode, parsed.Description)
	}
	return &parsed, nil
}

// Deliver routes one request. KYC goes to the admin DM only; every other
// category goes to its topic.
func (s *telegramSupportSink) Deliver(ctx context.Context, r SupportRequest) (SupportDeliveryResult, error) {
	if r.Category == "kyc" {
		return s.deliverKYCToAdmin(ctx, r)
	}
	return s.deliverToTopic(ctx, r)
}

// deliverKYCToAdmin sends the full details privately. Nothing is posted to the
// group for this category.
func (s *telegramSupportSink) deliverKYCToAdmin(ctx context.Context, r SupportRequest) (SupportDeliveryResult, error) {
	if s.adminUserID == "" {
		slog.Error("telegram: KYC request cannot be delivered - TELEGRAM_ADMIN_USER_ID is unset",
			"support_id", r.ID)
		return SupportDeliveryResult{}, fmt.Errorf("telegram admin user id not configured")
	}

	reporter := r.ReporterLogin
	if reporter == "" {
		reporter = "anonymous"
	}
	text := strings.Join([]string{
		"🪪 KYC / verification request",
		"",
		"From: " + reporter,
		"Support ID: " + r.ID.String(),
		"Page: " + valueOrDefault(r.PageURL, "unknown"),
		"",
		truncateRunes(r.Message, 3000),
	}, "\n")

	sent, err := s.sendMessage(ctx, s.adminUserID, "", text+screenshotNoteForDM(r))
	if err != nil {
		// Loud and specific. This is the case where the information reaches
		// nobody: there is no public post for KYC, so a failed DM means the
		// request exists only in the database, with no human notified.
		slog.Error("telegram: ADMIN DM FAILED - KYC request details reached nobody",
			"support_id", r.ID,
			"admin_user_id", s.adminUserID,
			"error", err,
			"hint", "if this is a 403 the admin has not started a chat with the bot, or has blocked it")
		return SupportDeliveryResult{}, err
	}

	// A KYC screenshot is the likeliest of all of them to show an identity
	// document, and this is the one path that never touches the group.
	s.attachScreenshot(ctx, r, s.adminUserID, "", messageIDOf(sent))
	return SupportDeliveryResult{}, nil
}

// deliverToTopic posts the redacted public message to the category's topic,
// falling back to General if the topic is gone.
func (s *telegramSupportSink) deliverToTopic(ctx context.Context, r SupportRequest) (SupportDeliveryResult, error) {
	title := supportCategoryTitles[r.Category]
	if title == "" {
		title = "💬 Support request"
	}
	// Public. Message, page and support id only - the support id is the link
	// back to the identity, which stays in the database.
	text := strings.Join([]string{
		title,
		"",
		truncateRunes(r.Message, 3000),
		"",
		"Page: " + valueOrDefault(r.PageURL, "unknown"),
		"Support ID: " + r.ID.String(),
	}, "\n")

	text += screenshotNoteForTopic(r)

	threadID := s.topics[r.Category]
	if threadID == "" {
		slog.Warn("telegram: no topic configured for category, using General",
			"category", r.Category, "support_id", r.ID)
		if _, err := s.sendMessage(ctx, s.chatID, "", text); err != nil {
			return SupportDeliveryResult{}, err
		}
		s.deliverScreenshotPrivately(ctx, r)
		return SupportDeliveryResult{RoutedToFallback: true}, nil
	}

	if _, err := s.sendMessage(ctx, s.chatID, threadID, text); err == nil {
		// Only after the report is delivered, and never into this chat: the
		// image goes to the admin DM. Nothing below this line can cost the
		// reporter their message.
		s.deliverScreenshotPrivately(ctx, r)
		return SupportDeliveryResult{}, nil
	} else {
		// A deleted topic, or a bot that lost can_manage_topics, must not lose
		// the report. Retry in General and mark it, because a report quietly
		// landing in General while its topic sits empty is invisible otherwise.
		slog.Error("telegram: topic post failed, falling back to General",
			"category", r.Category, "thread_id", threadID, "support_id", r.ID, "error", err)
	}

	if _, err := s.sendMessage(ctx, s.chatID, "", text); err != nil {
		slog.Error("telegram: General fallback also failed",
			"category", r.Category, "support_id", r.ID, "error", err)
		return SupportDeliveryResult{}, err
	}
	s.deliverScreenshotPrivately(ctx, r)
	return SupportDeliveryResult{RoutedToFallback: true}, nil
}

// telegramSinkConfigFrom builds the sink config from app config.
//
// Extracted so every caller reads the same seven variables. The KYC review
// alerter needs this sink too, and a second literal would be a second chance
// to forget TELEGRAM_ADMIN_USER_ID - which is the field that decides whether
// a KYC message reaches anybody at all.
func telegramSinkConfigFrom(cfg config.Config) telegramSinkConfig {
	return telegramSinkConfig{
		BotToken:    cfg.TelegramBotToken,
		ChatID:      cfg.TelegramChatID,
		AdminUserID: cfg.TelegramAdminUserID,
		TopicBugs:   cfg.TelegramTopicBugs,
		TopicKYC:    cfg.TelegramTopicKYC,
		TopicIdeas:  cfg.TelegramTopicIdeas,
		TopicHelp:   cfg.TelegramTopicHelp,
		TopicOther:  cfg.TelegramTopicOther,
	}
}

// messageIDOf reads the id of a message we just sent, for replying to it.
// Absent is not an error: the reply link is a convenience, and sendFile is
// told to send without it rather than fail.
func messageIDOf(resp *telegramResponse) int64 {
	if resp == nil || resp.Result == nil {
		return 0
	}
	return resp.Result.MessageID
}
