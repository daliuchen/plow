package main

import (
	"fmt"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"math/rand"
	"net/http"
)

func main() {
	// Echo instance
	e := echo.New()

	// Middleware
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	// Routes
	e.GET("/", hello)
	e.GET("/pt/:token", func(c echo.Context) error {
		token := c.Param("token")
		queryString := c.QueryString()
		codes := []int{
			http.StatusOK,
			http.StatusOK,
			http.StatusOK,
			http.StatusOK,
			http.StatusOK,
			http.StatusAccepted,
			http.StatusAlreadyReported,
			http.StatusBadRequest,
			http.StatusBadGateway,
			http.StatusContinue,
		}
		intn := rand.Intn(len(codes))
		return c.String(codes[intn], fmt.Sprintf("token:%s,query:%s", token, queryString))

	})

	// Start server
	e.Logger.Fatal(e.Start(":1323"))
}

// Handler
func hello(c echo.Context) error {
	return c.String(http.StatusOK, "Hello, World!")
}
