package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func TestExecuteArith(t *testing.T) {
	cases := []struct{ expr, want string }{
		{`{"expression":"2+2"}`, "4"},
		{`{"expression":"6*7"}`, "42"},
		{`{"expression":"(2+3)*4"}`, "20"},
		{`{"expression":"2**10"}`, "1024"},
		{`{"expression":"7//2"}`, "3"},
		{`{"expression":"7%3"}`, "1"},
		{`{"expression":"10/4"}`, "2.5"},
		{`{"expression":"-3+5"}`, "2"},
	}
	for _, c := range cases {
		got, err := Execute("arith", c.expr)
		if err != nil {
			t.Errorf("arith %s: %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("arith %s = %q, want %q", c.expr, got, c.want)
		}
	}
}

func TestExecuteRejectsNonWhitelistedOperation(t *testing.T) {
	if _, err := Execute("exec", `{}`); !errors.Is(err, ErrUnknownOperation) {
		t.Fatalf("expected ErrUnknownOperation, got %v", err)
	}
	if _, err := Execute("os.system", `{}`); !errors.Is(err, ErrUnknownOperation) {
		t.Fatalf("expected ErrUnknownOperation, got %v", err)
	}
}

func TestExecuteArithRejectsInjection(t *testing.T) {
	// Names, calls, attributes and subscripts are not in the grammar.
	for _, expr := range []string{
		`{"expression":"__import__('os')"}`,
		`{"expression":"foo(1)"}`,
		`{"expression":"a.b"}`,
		`{"expression":"a[0]"}`,
		`{"expression":"lambda: 1"}`,
	} {
		if _, err := Execute("arith", expr); err == nil {
			t.Errorf("expected rejection for %s", expr)
		}
	}
}

func TestExecuteArithDivisionByZero(t *testing.T) {
	for _, expr := range []string{`{"expression":"1/0"}`, `{"expression":"1//0"}`, `{"expression":"1%0"}`} {
		if _, err := Execute("arith", expr); err == nil {
			t.Errorf("expected division-by-zero error for %s", expr)
		}
	}
}

func TestExecuteDigest(t *testing.T) {
	got, err := Execute("digest", `{"data":"hello"}`)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("hello"))
	if got != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest = %q, want %q", got, hex.EncodeToString(sum[:]))
	}
}

func TestExecuteArithRejectsTrailingGarbage(t *testing.T) {
	if _, err := Execute("arith", `{"expression":"2+2; drop table"}`); err == nil {
		t.Fatal("expected rejection of trailing garbage")
	}
}

func TestKnownOperationsStable(t *testing.T) {
	names := strings.Join(OperationNames(), ",")
	if !strings.Contains(names, "arith") || !strings.Contains(names, "digest") {
		t.Fatalf("whitelist missing ops: %s", names)
	}
}
