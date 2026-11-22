// Package platform holds the cross-cutting pieces every domain package uses: the
// response envelope, typed API errors, the authenticated principal, tokens,
// idempotency, tenancy helpers and small crypto/ID utilities. It must not import any
// domain package, so every domain package can import it without cycles.
package platform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Success writes the standard success envelope {data, meta:{request_id}}.
func Success(c *gin.Context, status int, data any) { SuccessMeta(c, status, data, nil) }

// SuccessMeta adds extra fields (such as a pagination cursor) to the response meta.
func SuccessMeta(c *gin.Context, status int, data any, extra gin.H) {
	meta := gin.H{"request_id": c.GetString("request_id")}
	for k, v := range extra {
		meta[k] = v
	}
	c.JSON(status, gin.H{"data": data, "meta": meta})
}

// Fail writes the standard error envelope with empty details.
func Fail(c *gin.Context, status int, code, message string) {
	FailDetails(c, status, code, message, nil)
}

// FailDetails writes {error:{code,message,details}, meta:{request_id}}; details defaults to {}.
func FailDetails(c *gin.Context, status int, code, message string, details any) {
	if details == nil {
		details = gin.H{}
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": message, "details": details}, "meta": gin.H{"request_id": c.GetString("request_id")}})
}

// BadRequest answers 400 for a malformed body or field format.
func BadRequest(c *gin.Context) {
	Fail(c, http.StatusBadRequest, "INVALID_REQUEST", "The request body or fields are invalid.")
}

// NotFound answers 404 for anything missing or outside the caller's tenant scope.
func NotFound(c *gin.Context) {
	Fail(c, http.StatusNotFound, "NOT_FOUND", "The requested resource was not found.")
}

// Internal logs err and answers a generic 500 that never leaks internals.
func Internal(c *gin.Context, err error) {
	slog.Error("request failed", "request_id", c.GetString("request_id"), "error", err)
	Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "An unexpected error occurred.")
}

// Bind decodes the JSON body into dst, answering 400 on failure.
func Bind(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		BadRequest(c)
		return false
	}
	return true
}

// BindOptional decodes an optional JSON body into dst: an empty body leaves dst as it is,
// a malformed one answers 400.
func BindOptional(c *gin.Context, dst any) bool {
	raw, err := c.GetRawData()
	if err != nil || (len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, dst) != nil) {
		BadRequest(c)
		return false
	}
	return true
}

// Error is an API error a domain function can return; Respond turns it into the
// matching response. Any other error becomes a 500.
type Error struct {
	Status  int
	Code    string
	Message string
	Details any
}

func (e *Error) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

// Errf builds an *Error with a formatted message.
func Errf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Respond writes err as an API error: *Error keeps its status and code, anything else is a 500.
func Respond(c *gin.Context, err error) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		FailDetails(c, apiErr.Status, apiErr.Code, apiErr.Message, apiErr.Details)
		return
	}
	Internal(c, err)
}

// FieldError is one validation problem, addressed by a JSON path such as items[2].max_stack.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// FieldErrors collects validation problems.
type FieldErrors []FieldError

// Add records a problem at field.
func (e *FieldErrors) Add(field, format string, args ...any) {
	*e = append(*e, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

// Invalid builds a 422 *Error that carries the field errors in details.errors.
func Invalid(code, message string, errs []FieldError) *Error {
	return &Error{Status: http.StatusUnprocessableEntity, Code: code, Message: message, Details: gin.H{"errors": errs}}
}
