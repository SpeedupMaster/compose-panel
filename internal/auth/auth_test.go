package auth

import (
	"testing"
	"time"
)

func TestTOTPVerifyStep(t *testing.T) {
	sec := NewTOTPSecret()
	now := time.Unix(1_700_000_010, 0) // step = 56666667
	curStep := now.Unix() / 30

	for _, delta := range []int64{-1, 0, 1} {
		code, err := totpAt(sec, uint64(curStep+delta))
		if err != nil {
			t.Fatalf("totpAt(%d): %v", delta, err)
		}
		gotStep, ok := TOTPVerifyStep(sec, code, now)
		if !ok || gotStep != curStep+delta {
			t.Fatalf("delta %d: got (%d, %v), want (%d, true)", delta, gotStep, ok, curStep+delta)
		}
		if !TOTPValid(sec, code, now) {
			t.Fatalf("delta %d: TOTPValid returned false", delta)
		}
	}

	// Outside +/-1 step window must fail.
	for _, delta := range []int64{-2, 2} {
		code, err := totpAt(sec, uint64(curStep+delta))
		if err != nil {
			t.Fatalf("totpAt(%d): %v", delta, err)
		}
		if _, ok := TOTPVerifyStep(sec, code, now); ok {
			t.Fatalf("delta %d: expected TOTPVerifyStep to reject out-of-window code", delta)
		}
	}

	// Whitespace formatting is accepted.
	code, _ := totpAt(sec, uint64(curStep))
	if gotStep, ok := TOTPVerifyStep(sec, " "+code[:3]+" "+code[3:]+" ", now); !ok || gotStep != curStep {
		t.Fatalf("spaced code: got (%d, %v), want (%d, true)", gotStep, ok, curStep)
	}
}
