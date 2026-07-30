package main

import (
	"context"
	"net/http"
)

func contextWith(r *http.Request, c *Claims) context.Context {
	return context.WithValue(r.Context(), claimsKey, c)
}
