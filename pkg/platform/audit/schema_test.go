package audit

import (
	"context"
	"strings"
	"testing"
)

// TestEmbeddedSchemaCompiles asserts the schema shipped with the binary
// is valid JSON Schema. Catches malformed edits during review.
func TestEmbeddedSchemaCompiles(t *testing.T) {
	if _, err := compileSchema(EmbeddedEnvelopeSchema); err != nil {
		t.Fatalf("embedded schema failed to compile: %v", err)
	}
}

// TestValidatorAcceptsCanonical asserts a hand-built envelope that
// matches what audit.Emit produces passes validation.
func TestValidatorAcceptsCanonical(t *testing.T) {
	v, err := NewValidator(context.Background(), ValidatorConfig{}) // empty cfg -> embedded
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	good := []byte(`{
		"specversion": "1.0",
		"id": "9369c308-d96d-4905-9096-f369161cdb77",
		"source": "identity",
		"sourcekind": "service",
		"type": "identity.consumer.created",
		"subject": "acme-onboarding",
		"subjectkind": "consumer",
		"time": "2026-05-24T18:00:00.000Z",
		"correlationid": "abc-1234",
		"result": "ok",
		"data": {"consumer_id": "x"}
	}`)
	if err := v.Validate(good); err != nil {
		t.Fatalf("canonical envelope rejected: %v", err)
	}
}

// TestValidatorRejectsBadEnvelopes is the load-bearing case: drift in any
// shape-critical field must be caught at publish time.
func TestValidatorRejectsBadEnvelopes(t *testing.T) {
	v, err := NewValidator(context.Background(), ValidatorConfig{})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	cases := []struct {
		name     string
		envelope string
		wantHint string // substring that should appear in the validation error
	}{
		{
			name:     "unknown sourcekind",
			envelope: `{"specversion":"1.0","id":"9369c308-d96d-4905-9096-f369161cdb77","source":"x","sourcekind":"hacker","type":"x.y.z","subject":"","subjectkind":"","time":"2026-05-24T18:00:00Z","correlationid":"","result":"ok","data":null}`,
			wantHint: "sourcekind",
		},
		{
			name:     "unknown result enum",
			envelope: `{"specversion":"1.0","id":"9369c308-d96d-4905-9096-f369161cdb77","source":"x","sourcekind":"service","type":"x.y.z","subject":"","subjectkind":"","time":"2026-05-24T18:00:00Z","correlationid":"","result":"WAT","data":null}`,
			wantHint: "result",
		},
		{
			name:     "wrong specversion",
			envelope: `{"specversion":"2.0","id":"9369c308-d96d-4905-9096-f369161cdb77","source":"x","sourcekind":"service","type":"x.y.z","subject":"","subjectkind":"","time":"2026-05-24T18:00:00Z","correlationid":"","result":"ok","data":null}`,
			wantHint: "specversion",
		},
		{
			name:     "missing required type",
			envelope: `{"specversion":"1.0","id":"9369c308-d96d-4905-9096-f369161cdb77","source":"x","sourcekind":"service","subject":"","subjectkind":"","time":"2026-05-24T18:00:00Z","correlationid":"","result":"ok","data":null}`,
			wantHint: "type",
		},
		{
			name:     "additional property (registry-locked shape)",
			envelope: `{"specversion":"1.0","id":"9369c308-d96d-4905-9096-f369161cdb77","source":"x","sourcekind":"service","type":"x.y.z","subject":"","subjectkind":"","time":"2026-05-24T18:00:00Z","correlationid":"","result":"ok","data":null,"backdoor":"yes"}`,
			wantHint: "backdoor",
		},
		{
			name:     "id not a uuid",
			envelope: `{"specversion":"1.0","id":"not-a-uuid","source":"x","sourcekind":"service","type":"x.y.z","subject":"","subjectkind":"","time":"2026-05-24T18:00:00Z","correlationid":"","result":"ok","data":null}`,
			wantHint: "id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := v.Validate([]byte(tc.envelope))
			if err == nil {
				t.Fatalf("expected validation error, got nil for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantHint) {
				t.Logf("error: %v", err)
				t.Fatalf("error did not mention %q; got: %v", tc.wantHint, err)
			}
		})
	}
}
