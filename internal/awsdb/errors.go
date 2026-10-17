package awsdb

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/aws/smithy-go"
)

type callError struct {
	text string
	err  error
}

func (e *callError) Error() string { return e.text }
func (e *callError) Unwrap() error { return e.err }

// CallFailed describes a failed AWS call by AWS's own message, with the hint
// that fits: denied for authorization failures, other for other AWS errors.
func CallFailed(call string, err error, denied, other string) error {
	if problem, ok := transportProblem(err); ok {
		return &callError{text: call + " " + problem, err: err}
	}
	var apiError smithy.APIError
	if !errors.As(err, &apiError) {
		return &callError{text: call + " failed: " + err.Error(), err: err}
	}
	message := strings.TrimSuffix(apiError.ErrorMessage(), ".")
	if message == "" {
		message = apiError.ErrorCode()
	}
	if accessDenied(apiError.ErrorCode()) {
		return &callError{text: sentences(call+" was denied: "+message, denied), err: err}
	}
	return &callError{text: sentences(call+" failed: "+message, other), err: err}
}

func sentences(first, second string) string {
	if second == "" {
		return first
	}
	return first + ". " + second
}

func accessDenied(code string) bool {
	return code == "AccessDenied" || code == "AccessDeniedException" || code == "UnauthorizedOperation"
}

const networkHint = "Check network access to AWS (VPN, proxy, firewall)"

// transportProblem describes failures to get any answer from AWS.
func transportProblem(err error) (string, bool) {
	var urlError *url.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out. " + networkHint, true
	case errors.Is(err, context.Canceled):
		return "was canceled", true
	case errors.As(err, &urlError):
		return "could not reach AWS: " + urlError.Err.Error() + ". " + networkHint, true
	}
	return "", false
}
