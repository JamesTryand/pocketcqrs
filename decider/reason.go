package decider

import (
	"errors"
	"strings"
	"unicode"
)

// The machine-readable reasons a refusal can carry beside its message
// (command gateway contract v0.1, G5). A caller decides on the reason, never
// on the words.
const (
	// ReasonDoesNotExist: the stream the command needs is not there yet.
	ReasonDoesNotExist = "does-not-exist"
	// ReasonAlreadyExists: a create sent to a stream that already exists.
	ReasonAlreadyExists = "already-exists"
)

// AggregateWords renders an aggregate name as the lower-case words the
// generated refusals use: "customerContact" is "customer contact", and an
// all-capitals run such as "SKU" is kept. scaffold.Words is this function.
func AggregateWords(name string) string {
	runes := []rune(name)
	var words []string
	start := 0
	for i := 1; i < len(runes); i++ {
		nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
		if unicode.IsUpper(runes[i]) && (!unicode.IsUpper(runes[i-1]) || nextLower) {
			words = append(words, string(runes[start:i]))
			start = i
		}
	}
	words = append(words, string(runes[start:]))
	for i, w := range words {
		if len([]rune(w)) < 2 || strings.ToUpper(w) != w {
			words[i] = strings.ToLower(w)
		}
	}
	return strings.Join(words, " ")
}

// DoesNotExistMessage is the refusal a generated decider gives a command
// that needs the aggregate's stream when there is none yet.
func DoesNotExistMessage(aggregate string) string {
	return AggregateWords(aggregate) + " does not exist"
}

// AlreadyExistsMessage is the refusal a generated decider gives a create sent
// to a stream that already exists.
func AlreadyExistsMessage(aggregate string) string {
	return AggregateWords(aggregate) + " already exists"
}

// ReasonedError is a refusal that names its own reason. A hand-written Go
// decider returns one (Refuse) when a caller needs to tell this refusal from
// others without reading its words.
type ReasonedError struct {
	Reason  string
	Message string
}

func (e *ReasonedError) Error() string { return e.Message }

// Refuse returns a refusal carrying reason beside message.
func Refuse(reason, message string) error {
	return &ReasonedError{Reason: reason, Message: message}
}

// RefusalReason is the machine-readable reason for a decider's refusal of a
// command on aggregate, or "" when it has none: the reason a ReasonedError
// names; otherwise does-not-exist or already-exists when the message is
// exactly one of the two refusals the generators write for that aggregate
// (DoesNotExistMessage, AlreadyExistsMessage). Matching the whole message
// means deciders generated before this existed get the reason without being
// generated again, and no other refusal is mistaken for one of them.
func RefusalReason(err error, aggregate string) string {
	var r *ReasonedError
	if errors.As(err, &r) {
		return r.Reason
	}
	if err == nil {
		return ""
	}
	switch err.Error() {
	case DoesNotExistMessage(aggregate):
		return ReasonDoesNotExist
	case AlreadyExistsMessage(aggregate):
		return ReasonAlreadyExists
	}
	return ""
}
