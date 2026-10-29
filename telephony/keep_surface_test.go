package telephony_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/iansmith/aatoolkit/telephony"
)

// The surface a depending project imports from this package, pinned from
// outside it the way that project sees it (AATK-164). Each declaration below
// fails to compile if the symbol it names is removed, renamed, re-typed, or
// changes between typed and untyped; the const blocks also fail to compile if a
// value moves. TestKeepSurface_Values covers what a compile-time check cannot.

var (
	_ func(int16) byte = telephony.LinearToMuLaw

	_ byte = telephony.MuLawSilence

	_ telephony.TransportType = telephony.TransportVoice
	_ telephony.TransportType = telephony.TransportSMS

	// A conversion between struct types compiles only when the field names,
	// types and order are identical, so a field added, dropped, renamed or
	// re-typed on Message breaks this line.
	_ = struct {
		Text       string
		Transport  telephony.TransportType
		SessionID  string
		From       string
		MessageSID string
	}(telephony.Message{})

	// Assignable both ways, so User's method set is exactly Context() string.
	_ interface{ Context() string } = telephony.User(nil)
	_ telephony.User                = (interface{ Context() string })(nil)

	// Untyped constants: each is assignable to int and to time.Duration. A
	// typed int breaks the time.Duration line, which is how realtime code
	// scales MarkEchoGraceMS by time.Millisecond.
	_ int           = telephony.SampleRateHz
	_ time.Duration = telephony.SampleRateHz
	_ int           = telephony.MuLawFrameMS
	_ time.Duration = telephony.MuLawFrameMS
	_ int           = telephony.MarkEchoGraceMS
	_ time.Duration = telephony.MarkEchoGraceMS
)

// TransportType's underlying type is string.
const _ telephony.TransportType = ""

// Value pins. Each pair overflows uint, a compile error, unless the constant is
// exactly the stated value. MuLawSilence needs one line: it is a byte, so
// nothing above 0xFF exists.
const (
	_ byte = telephony.MuLawSilence - 0xFF

	_ uint = telephony.SampleRateHz - 8000
	_ uint = 8000 - telephony.SampleRateHz

	_ uint = telephony.MuLawFrameMS - 20
	_ uint = 20 - telephony.MuLawFrameMS

	_ uint = telephony.MarkEchoGraceMS - 500
	_ uint = 500 - telephony.MarkEchoGraceMS
)

// TestKeepSurface_Values pins the two properties above that no compile-time
// check sees: that MuLawSilence is a typed byte rather than an untyped constant
// (an untyped one also satisfies `var _ byte = ...`), and the transport names'
// string values, which a depending project may compare against or persist.
func TestKeepSurface_Values(t *testing.T) {
	if got, want := reflect.TypeOf(telephony.MuLawSilence), reflect.TypeOf(byte(0)); got != want {
		t.Errorf("MuLawSilence has type %v, want %v", got, want)
	}
	if telephony.TransportVoice != "voice" {
		t.Errorf("TransportVoice = %q, want %q", telephony.TransportVoice, "voice")
	}
	if telephony.TransportSMS != "sms" {
		t.Errorf("TransportSMS = %q, want %q", telephony.TransportSMS, "sms")
	}
}
