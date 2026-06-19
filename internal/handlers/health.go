package handlers

import "github.com/gofiber/fiber/v2"

const ServiceName = "grainlify-api"

func Health() fiber.Handler {
	return func(c *fiber.Ctx) error {
		return c.Status(fiber.StatusOK).JSON(fiber.Map{
			"ok":      true,
			"service": ServiceName,
		})
	}
}








