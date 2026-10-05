package pii

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/kms"
)

// Deps is what a generated decider.Protector needs at runtime. KMS is
// required; Cache and Subjects are optional (nil: no warm reads, and no
// local erased-subject check — the facade's 409 tombstone still refuses).
type Deps struct {
	KMS      kms.Client
	Cache    *Cache
	Subjects SubjectStatus
}

// checkSubject refuses fresh personal data for an empty or erased subject.
func (d Deps) checkSubject(ctx context.Context, subject, field, subjectField string) error {
	if subject == "" {
		return fmt.Errorf("pii: %s is personal data but its piiSubject %s is empty", field, subjectField)
	}
	if d.Subjects == nil {
		return nil
	}
	gone, err := d.Subjects.IsErased(ctx, subject)
	if err != nil {
		return err
	}
	if gone {
		return &kms.SubjectErasedError{SubjectID: subject}
	}
	return nil
}

// ProtectField is a generated Protect's per-field step: it encrypts a Fresh
// value under subject (named by the field's piiSubject sibling), and checks
// that a value already sealed — copied from a sealed command or from state —
// is sealed under that same subject, failing closed otherwise rather than
// storing data under the wrong key. field and subjectField name the two
// fields in errors ("OrderPlaced.email", "customerId").
func ProtectField[T any](ctx context.Context, d Deps, v Value[T], subject, field, subjectField string) (Value[T], error) {
	switch v.state {
	case Unset:
		return v, nil
	case Fresh:
		if err := d.checkSubject(ctx, subject, field, subjectField); err != nil {
			return v, err
		}
		return v.Encrypt(ctx, d.KMS, subject, d.Cache)
	case Redacted:
		return v, &kms.SubjectErasedError{SubjectID: v.subject}
	}
	if v.subject != subject {
		return v, fmt.Errorf("pii: %s is sealed under subject %q but its piiSubject %s is %q; refusing to store it under the wrong key",
			field, v.subject, subjectField, subject)
	}
	return v, nil
}

// SealField names one top-level personal-data field of a command payload
// and the sibling field holding its subject id.
type SealField struct {
	Field, Subject string
}

// SealPayload is a generated Sealer's body: it replaces each named field's
// plaintext in payload with an envelope encrypted under its subject, one
// encrypt-batch call per subject, priming the cache with what it sealed. A
// field that is absent or null is left alone; one already an envelope (a
// reactor carrying ciphertext along) is kept if its subject matches.
// Anything that isn't a JSON object is returned unchanged for decide to
// reject.
func SealPayload(ctx context.Context, d Deps, payload json.RawMessage, fields []SealField) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil || obj == nil {
		return payload, nil
	}
	type item struct {
		field string
		plain []byte
	}
	bySubject := map[string][]item{}
	for _, f := range fields {
		raw, ok := obj[f.Field]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		var subject string
		if err := json.Unmarshal(obj[f.Subject], &subject); err != nil {
			return nil, fmt.Errorf("pii: %s is personal data but its piiSubject %s is not a string", f.Field, f.Subject)
		}
		if IsEnvelope(raw) {
			s, _, _, err := ParseEnvelope(raw)
			if err != nil {
				return nil, err
			}
			if s != subject {
				return nil, fmt.Errorf("pii: %s is sealed under subject %q but its piiSubject %s is %q", f.Field, s, f.Subject, subject)
			}
			continue
		}
		if err := d.checkSubject(ctx, subject, f.Field, f.Subject); err != nil {
			return nil, err
		}
		bySubject[subject] = append(bySubject[subject], item{field: f.Field, plain: append([]byte(nil), bytes.TrimSpace(raw)...)})
	}
	if len(bySubject) == 0 {
		return payload, nil
	}
	subjects := make([]string, 0, len(bySubject))
	for s := range bySubject {
		subjects = append(subjects, s)
	}
	sort.Strings(subjects)
	for _, subject := range subjects {
		items := bySubject[subject]
		if err := d.KMS.EnsureKey(ctx, subject); err != nil {
			return nil, err
		}
		plains := make([][]byte, len(items))
		for i, it := range items {
			plains[i] = it.plain
		}
		res, err := d.KMS.EncryptBatch(ctx, subject, plains)
		if err != nil {
			return nil, err
		}
		if len(res) != len(items) {
			return nil, &kms.ProtocolError{Msg: "encrypt-batch answered the wrong number of items"}
		}
		for i, it := range items {
			if !res[i].Succeeded() {
				return nil, &kms.ProtocolError{Msg: fmt.Sprintf("encrypt-batch item error for subject %q: %s", subject, res[i].Err)}
			}
			cipher := string(res[i].Value)
			if d.Cache != nil {
				d.Cache.Put(subject, cipher, it.plain)
			}
			env, err := json.Marshal(FromCiphertext[json.RawMessage](subject, cipher))
			if err != nil {
				return nil, err
			}
			obj[it.field] = env
		}
	}
	return json.Marshal(obj)
}

// CommandReveals collects a command's sealed values for one Buffer flush.
type CommandReveals struct {
	got map[[2]string]decider.Reveal
}

// QueueCommand queues every envelope anywhere in cmd's payload into buf.
// After buf.Flush, Apply attaches what was revealed to the command.
func QueueCommand(buf *Buffer, cmd decider.Command) *CommandReveals {
	cr := &CommandReveals{got: map[[2]string]decider.Reveal{}}
	var root any
	if err := json.Unmarshal(cmd.Payload, &root); err != nil {
		return cr
	}
	walkEnvelopes(root, func(subject, cipher string) {
		key := [2]string{subject, cipher}
		buf.RevealRaw(subject, cipher, func(plain []byte, redacted bool) {
			cr.got[key] = decider.Reveal{Plaintext: plain, Redacted: redacted}
		})
	})
	return cr
}

// Apply returns cmd carrying the reveals collected so far.
func (cr *CommandReveals) Apply(cmd decider.Command) decider.Command {
	if len(cr.got) == 0 {
		return cmd
	}
	return cmd.WithReveals(cr.got)
}

func walkEnvelopes(v any, visit func(subject, cipher string)) {
	switch t := v.(type) {
	case map[string]any:
		if body, ok := t[envelopeKey].(map[string]any); ok && len(t) == 1 {
			s, sok := body[subjectKey].(string)
			c, cok := body[cipherKey].(string)
			if sok && cok {
				visit(s, c)
			}
			return
		}
		for _, child := range t {
			walkEnvelopes(child, visit)
		}
	case []any:
		for _, child := range t {
			walkEnvelopes(child, visit)
		}
	}
}

// ResolveIn is what a generated Decide calls on each personal-data field of
// its command payload: a pure lookup of the reveals the shell attached to
// cmd, turning a Pending value into Known (or Redacted). Without a reveal
// the value stays Pending, so reading it asks the shell for one.
func ResolveIn[T any](cmd decider.Command, p *Value[T]) error {
	if p.state != Pending {
		return nil
	}
	r, ok := cmd.Revealed(p.subject, p.cipher)
	if !ok {
		return nil
	}
	if r.Redacted {
		*p = NewRedacted[T](p.subject)
		return nil
	}
	v, err := decode[T](r.Plaintext)
	if err != nil {
		return err
	}
	*p = Value[T]{state: Known, subject: p.subject, cipher: p.cipher, v: v}
	return nil
}
