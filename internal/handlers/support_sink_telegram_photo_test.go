package handlers

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// pngOf builds a real PNG of the given dimensions. Real bytes rather than a
// stub, because the decision this code makes is read out of the image header -
// a fake would exercise the branch without testing what selects it.
func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{1, 2, 3, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return b.Bytes()
}

func withScreenshot(r SupportRequest, data []byte) SupportRequest {
	r.ScreenshotBytes, r.ScreenshotExt = data, "png"
	return r
}

func findSend(sends []capturedSend, method string) *capturedSend {
	for i := range sends {
		if sends[i].Method == method {
			return &sends[i]
		}
	}
	return nil
}

// The reported bug: the Discord message carried the screenshot, the Telegram
// one did not, because the sink had no image path at all.
//
// The report goes to its public topic; the IMAGE goes to the admin DM. What a
// screenshot contains cannot be known before it posts - submissions have
// arrived carrying an unrelated chat, and a billing page with a name and an
// amount on it - and @Grainlify is readable without joining.
func TestTelegramSink_SendsTheReportToTheTopicAndTheImageToTheDM(t *testing.T) {
	sink, sends, closeSrv := fakeTelegram(t, nil)
	defer closeSrv()

	shot := pngOf(t, 1200, 800)
	req := withScreenshot(sampleRequest("bug"), shot)
	if _, err := sink.Deliver(context.Background(), req); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if len(*sends) != 2 {
		t.Fatalf("made %d calls, want the report then the image", len(*sends))
	}
	// Order matters and is the failure isolation: by the time anything can go
	// wrong with the image, the report has already arrived.
	report := (*sends)[0]
	if report.Method != "sendMessage" {
		t.Errorf("first call was %s, want the report text first", report.Method)
	}
	if report.ChatID != "-100999" || report.ThreadID != "1700" {
		t.Errorf("report went to chat %q thread %q, want the bugs topic", report.ChatID, report.ThreadID)
	}

	photo := (*sends)[1]
	if photo.Method != "sendPhoto" {
		t.Fatalf("second call was %s, want sendPhoto", photo.Method)
	}
	if photo.ChatID != "42" {
		t.Errorf("image went to chat %q, want the admin DM", photo.ChatID)
	}
	if photo.FileField != "photo" || photo.FileBytes != len(shot) {
		t.Errorf("uploaded %s of %d bytes, want the whole image as a photo", photo.FileField, photo.FileBytes)
	}
	// The support id is the ONLY thing tying the image to its report now that
	// the two are in different chats - no reply link can span them. It has to
	// appear in both.
	if !strings.Contains(photo.Caption, req.ID.String()) {
		t.Errorf("caption %q does not carry the support id", photo.Caption)
	}
	if !strings.Contains(report.Text, req.ID.String()) {
		t.Errorf("topic post does not carry the support id, so the image cannot be matched to it")
	}
}

// The property this whole design exists for.
func TestTelegramSink_NoScreenshotEverReachesThePublicGroup(t *testing.T) {
	sink, sends, closeSrv := fakeTelegram(t, nil)
	defer closeSrv()

	for _, category := range []string{"bug", "idea", "help", "other", "kyc"} {
		if _, err := sink.Deliver(context.Background(),
			withScreenshot(sampleRequest(category), pngOf(t, 900, 600))); err != nil {
			t.Fatalf("deliver %s: %v", category, err)
		}
	}

	uploads := 0
	for _, c := range *sends {
		if c.Method != "sendPhoto" && c.Method != "sendDocument" {
			continue
		}
		uploads++
		if c.ChatID != "42" {
			t.Errorf("a %s upload went to chat %q - @Grainlify is public and readable without joining",
				c.Method, c.ChatID)
		}
	}
	if uploads != 5 {
		t.Errorf("%d images uploaded for 5 reports; every one should reach the admin", uploads)
	}
}

// A topic reader has to know an image exists, or somebody re-asks the reporter
// for something they already sent.
func TestTelegramSink_TopicSaysAScreenshotExistsWithoutShowingIt(t *testing.T) {
	sink, sends, closeSrv := fakeTelegram(t, nil)
	defer closeSrv()

	if _, err := sink.Deliver(context.Background(),
		withScreenshot(sampleRequest("bug"), pngOf(t, 900, 600))); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	topic := findSend(*sends, "sendMessage")
	if topic == nil {
		t.Fatal("no topic post")
	}
	if !strings.Contains(topic.Text, "screenshot") && !strings.Contains(topic.Text, "Screenshot") {
		t.Errorf("the topic post does not mention the screenshot:\n%s", topic.Text)
	}
	// And it must not claim the image is in the topic.
	if strings.Contains(topic.Text, "below") {
		t.Errorf("the topic post says the image is below it, which it is not:\n%s", topic.Text)
	}
}

func TestTelegramSink_NoScreenshotMeansNoSecondCall(t *testing.T) {
	sink, sends, closeSrv := fakeTelegram(t, nil)
	defer closeSrv()

	if _, err := sink.Deliver(context.Background(), sampleRequest("bug")); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(*sends) != 1 {
		t.Fatalf("made %d calls for a report with no image, want 1", len(*sends))
	}
}

// The constraint that actually bites. Telegram allows a 10 MB photo - above
// our own 5 MB cap - but width+height must not exceed 10000, and a full-page
// screenshot breaks that at a fraction of a megabyte.
func TestTelegramSink_TallScreenshotIsSentAsAFileRatherThanDropped(t *testing.T) {
	sink, sends, closeSrv := fakeTelegram(t, nil)
	defer closeSrv()

	tall := pngOf(t, 1512, 9000) // 10512 in total
	if _, err := sink.Deliver(context.Background(), withScreenshot(sampleRequest("bug"), tall)); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if findSend(*sends, "sendPhoto") != nil {
		t.Error("tried sendPhoto for an image Telegram documents as too large; the round trip is avoidable")
	}
	doc := findSend(*sends, "sendDocument")
	if doc == nil {
		t.Fatal("the screenshot was dropped instead of being sent as a file")
	}
	if doc.FileField != "document" {
		t.Errorf("file field = %q, want document", doc.FileField)
	}
	if doc.ChatID != "42" {
		t.Errorf("the file went to chat %q, want the admin DM", doc.ChatID)
	}
	// A downgrade nobody can see is indistinguishable from a bug.
	if !strings.Contains(doc.Caption, "1512x9000") {
		t.Errorf("caption does not say why it arrived as a file: %q", doc.Caption)
	}
}

func TestTelegramPhotoRejection_NamesTheLimitItBreaks(t *testing.T) {
	if why := telegramPhotoRejection(pngOf(t, 1200, 800)); why != "" {
		t.Errorf("an ordinary screenshot was rejected: %s", why)
	}
	// Exactly at the documented boundary: 10000 in total is allowed.
	if why := telegramPhotoRejection(pngOf(t, 5000, 5000)); why != "" {
		t.Errorf("width+height of exactly 10000 was rejected: %s", why)
	}
	if why := telegramPhotoRejection(pngOf(t, 5000, 5001)); why == "" {
		t.Error("width+height of 10001 was accepted")
	}
	// Ratio: 20:1 is allowed, 21:1 is not.
	if why := telegramPhotoRejection(pngOf(t, 2000, 100)); why != "" {
		t.Errorf("a 20:1 image was rejected: %s", why)
	}
	if why := telegramPhotoRejection(pngOf(t, 2100, 100)); why == "" {
		t.Error("a 21:1 image was accepted")
	}
	// Unreadable header: send as a file rather than assume it is fine.
	if why := telegramPhotoRejection([]byte("not an image at all")); why == "" {
		t.Error("an unreadable image was treated as a valid photo")
	}
}

// The hard requirement: a screenshot that cannot be sent must not fail the
// whole delivery. The text still has to arrive.
func TestTelegramSink_AFailedScreenshotDoesNotFailTheReport(t *testing.T) {
	sink, sends, closeSrv := fakeTelegram(t, func(c capturedSend) (int, string) {
		if c.Method == "sendPhoto" || c.Method == "sendDocument" {
			return 400, `{"ok":false,"error_code":400,"description":"PHOTO_INVALID_DIMENSIONS"}`
		}
		return 200, `{"ok":true,"result":{"message_id":7}}`
	})
	defer closeSrv()

	res, err := sink.Deliver(context.Background(), withScreenshot(sampleRequest("bug"), pngOf(t, 1200, 800)))
	if err != nil {
		t.Fatalf("a rejected screenshot failed the whole delivery: %v", err)
	}
	if res.RoutedToFallback {
		t.Error("an image failure was reported as a topic-routing failure")
	}

	// And the gap is stated to the person who was supposed to receive the
	// image, rather than only in a log nobody is reading.
	var note *capturedSend
	for i, c := range *sends {
		if c.Method == "sendMessage" && strings.Contains(c.Text, "could not be delivered") {
			note = &(*sends)[i]
		}
	}
	if note == nil {
		t.Fatal("the screenshot vanished silently; nobody can tell one was attached")
	}
	if note.ChatID != "42" {
		t.Errorf("the failure note went to chat %q, want the admin DM", note.ChatID)
	}
}

// KYC is the category most likely to carry an identity document.
func TestTelegramSink_KYCScreenshotGoesToTheDMAndNeverToTheGroup(t *testing.T) {
	sink, sends, closeSrv := fakeTelegram(t, nil)
	defer closeSrv()

	if _, err := sink.Deliver(context.Background(), withScreenshot(sampleRequest("kyc"), pngOf(t, 800, 600))); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	photo := findSend(*sends, "sendPhoto")
	if photo == nil {
		t.Fatal("the admin got no screenshot for a KYC request")
	}
	if photo.ChatID != "42" {
		t.Errorf("KYC screenshot went to chat %q, want the admin DM", photo.ChatID)
	}
	for _, c := range *sends {
		if c.ChatID == "-100999" {
			t.Fatalf("a KYC %s reached the public group", c.Method)
		}
	}
}

// The caption is a label, not the report: Telegram caps it at 1024 characters
// and the report runs to about 3100, which is why the text is a separate
// message rather than a caption.
func TestTelegramSink_CaptionStaysWithinTheLimit(t *testing.T) {
	sink, sends, closeSrv := fakeTelegram(t, nil)
	defer closeSrv()

	r := withScreenshot(sampleRequest("bug"), pngOf(t, 900, 700))
	r.Message = strings.Repeat("a very long report. ", 400) // ~8000 chars
	if _, err := sink.Deliver(context.Background(), r); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	photo := findSend(*sends, "sendPhoto")
	if photo == nil {
		t.Fatal("no photo sent")
	}
	if n := len([]rune(photo.Caption)); n > 1024 {
		t.Errorf("caption is %d runes, Telegram allows 1024", n)
	}
	// The report itself must not have been squeezed into the caption.
	text := findSend(*sends, "sendMessage")
	if text == nil || len([]rune(text.Text)) < 2000 {
		t.Error("the report text was not sent in full as its own message")
	}
}

// The misconfiguration case. With no admin id there is nowhere private to send
// an image, and the tempting fallback - post it to the group so at least
// somebody sees it - is the exact disclosure this design exists to prevent.
// Losing the image is the correct outcome; the report itself still arrives.
func TestTelegramSink_WithNoAdminIDTheImageIsDroppedRatherThanPublished(t *testing.T) {
	sink, sends, closeSrv := fakeTelegram(t, nil, withoutAdminUserID)
	defer closeSrv()

	res, err := sink.Deliver(context.Background(),
		withScreenshot(sampleRequest("bug"), pngOf(t, 900, 600)))
	if err != nil {
		t.Fatalf("a missing admin id failed the whole report: %v", err)
	}
	if res.RoutedToFallback {
		t.Error("a missing admin id was reported as a topic-routing failure")
	}

	for _, c := range *sends {
		if c.Method == "sendPhoto" || c.Method == "sendDocument" {
			t.Errorf("an image was uploaded to chat %q with no admin id configured", c.ChatID)
		}
	}
	// The report still reaches its topic - only the image is lost.
	if topic := findSend(*sends, "sendMessage"); topic == nil || topic.ChatID != "-100999" {
		t.Error("the report did not reach its topic")
	}
}
