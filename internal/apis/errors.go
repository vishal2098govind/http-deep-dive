package apis

import "fmt"

type Error struct {
	StatusCode int
	Message    string
	Metadata   string
}

func (e Error) Error() string {
	return e.Message
}

func NewError(statusCode int, msg string) Error {
	return Error{StatusCode: statusCode, Message: msg}
}

func NewErrorWithMetadata(statusCode int, msg string, metadata string) Error {
	return Error{StatusCode: statusCode, Message: msg, Metadata: metadata}
}

func NewErrorf(statusCode int, msg string, metadata string, a ...any) Error {
	return Error{StatusCode: statusCode, Message: msg, Metadata: fmt.Sprintf(metadata, a...)}
}
