package stats

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func mustNoError(t testing.TB, err error, context ...any) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error%s: %v", formatTestContext(context), err)
	}
}

func mustEqual(t testing.TB, expected, actual any, context ...any) {
	t.Helper()
	if !reflect.DeepEqual(expected, actual) {
		t.Fatalf("expected %v, got %v%s", expected, actual, formatTestContext(context))
	}
}

func mustNotEqual(t testing.TB, unexpected, actual any) {
	t.Helper()
	if reflect.DeepEqual(unexpected, actual) {
		t.Fatalf("did not expect %v", actual)
	}
}

func mustTrue(t testing.TB, value bool) {
	t.Helper()
	if !value {
		t.Fatal("expected true")
	}
}

func mustFalse(t testing.TB, value bool) {
	t.Helper()
	if value {
		t.Fatal("expected false")
	}
}

func mustInDelta(t testing.TB, expected, actual, delta float64) {
	t.Helper()
	if math.Abs(expected-actual) > delta {
		t.Fatalf("expected %v ± %v, got %v", expected, delta, actual)
	}
}

func mustNil(t testing.TB, value any) {
	t.Helper()
	if !isNil(value) {
		t.Fatalf("expected nil, got %v", value)
	}
}

func mustNotNil(t testing.TB, value any) {
	t.Helper()
	if isNil(value) {
		t.Fatal("expected non-nil value")
	}
}

func mustContain(t testing.TB, value, substring string) {
	t.Helper()
	if !strings.Contains(value, substring) {
		t.Fatalf("expected %q to contain %q", value, substring)
	}
}

func mustNotContain(t testing.TB, value, substring string) {
	t.Helper()
	if strings.Contains(value, substring) {
		t.Fatalf("expected %q not to contain %q", value, substring)
	}
}

func mustZero(t testing.TB, value any, context ...any) {
	t.Helper()
	if value == nil || !reflect.ValueOf(value).IsZero() {
		t.Fatalf("expected zero, got %v%s", value, formatTestContext(context))
	}
}

func mustErrorIs(t testing.TB, err, target error, context ...any) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("expected error %v, got %v%s", target, err, formatTestContext(context))
	}
}

func mustRegexp(t testing.TB, expression, value string) {
	t.Helper()
	matched, err := regexp.MatchString(expression, value)
	if err != nil {
		t.Fatalf("invalid regexp %q: %v", expression, err)
	}
	if !matched {
		t.Fatalf("expected %q to match %q", value, expression)
	}
}

func mustPanicsWithError(t testing.TB, expected string, function func()) {
	t.Helper()
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("expected panic")
		}
		actual, ok := recovered.(error)
		if !ok {
			t.Fatalf("expected error panic, got %T: %v", recovered, recovered)
		}
		if actual.Error() != expected {
			t.Fatalf("expected panic %q, got %q", expected, actual.Error())
		}
	}()
	function()
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func formatTestContext(context []any) string {
	if len(context) == 0 {
		return ""
	}
	return ": " + fmt.Sprint(context...)
}
