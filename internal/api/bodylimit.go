package api

import (
	"io"
	"strings"

	"github.com/gofiber/fiber/v2"
)

type BodyLimitConfig struct {
	DefaultLimit    int
	WebhookLimit    int
	WebhookPrefixes []string
}

func NewBodyLimitMiddleware(cfg BodyLimitConfig) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if cfg.DefaultLimit <= 0 {
			return c.Next()
		}

		method := c.Method()
		if method == fiber.MethodGet || method == fiber.MethodHead || method == fiber.MethodOptions {
			return c.Next()
		}

		path := c.Path()
		limit := cfg.DefaultLimit
		for _, prefix := range cfg.WebhookPrefixes {
			if strings.HasPrefix(path, prefix) {
				limit = cfg.WebhookLimit
				break
			}
		}

		contentLength := c.Request().Header.ContentLength()
		if contentLength > int64(limit) {
			return fiber.NewError(fiber.StatusRequestEntityTooLarge, "Request body exceeds size limit")
		}

		if stream := c.Request().BodyStream(); stream != nil {
			c.Request().SetBodyStream(&limitedBodyReader{reader: stream, limit: int64(limit)}, limit)
		} else if len(c.Request().Body()) > limit {
			return fiber.NewError(fiber.StatusRequestEntityTooLarge, "Request body exceeds size limit")
		}

		return c.Next()
	}
}

type limitedBodyReader struct {
	reader io.Reader
	limit  int64
	read   int64
}

func (r *limitedBodyReader) Read(p []byte) (int, error) {
	if r.read >= r.limit {
		return 0, fiber.ErrRequestEntityTooLarge
	}
	maxRead := r.limit - r.read
	if int64(len(p)) > maxRead {
		p = p[:maxRead]
	}
	n, err := r.reader.Read(p)
	r.read += int64(n)
	return n, err
}

func (r *limitedBodyReader) Close() error {
	if closer, ok := r.reader.(io.ReadCloser); ok {
		return closer.Close()
	}
	return nil
}
