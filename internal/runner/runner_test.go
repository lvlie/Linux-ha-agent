package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLocalRun(t *testing.T) {
	ctx := context.Background()
	res, err := Local{}.Run(ctx, Command("sh", "-c", `echo "$FOO:$LC_ALL"; echo oops >&2; exit 3`).WithEnv("FOO", "bar"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "bar:C\n" || res.Stderr != "oops\n" || res.ExitCode != 3 {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestLocalMissingBinary(t *testing.T) {
	res, err := Local{}.Run(context.Background(), Command("definitely-not-a-command-xyz"))
	if err != nil || res.ExitCode != 127 {
		t.Fatalf("expected exit 127, got %+v %v", res, err)
	}
}

func TestLocalContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Local{}.Run(ctx, Command("sleep", "5"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestCheck(t *testing.T) {
	f := (&Fake{}).On("false", Result{ExitCode: 1, Stderr: "bad thing"})
	_, err := Check(context.Background(), f, Command("false"))
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || !strings.Contains(err.Error(), "bad thing") {
		t.Fatalf("expected ExitError, got %v", err)
	}
}

func TestHas(t *testing.T) {
	if !Has(context.Background(), Local{}, "sh") {
		t.Fatal("sh should exist")
	}
	if Has(context.Background(), Local{}, "definitely-not-a-command-xyz") {
		t.Fatal("unexpected command found")
	}
}

func TestWithEnvDoesNotMutate(t *testing.T) {
	base := Command("x").WithEnv("A", "1")
	derived := base.WithEnv("B", "2")
	if _, ok := base.Env["B"]; ok {
		t.Fatal("WithEnv mutated the original command")
	}
	if derived.Env["A"] != "1" || derived.Env["B"] != "2" {
		t.Fatalf("unexpected env %v", derived.Env)
	}
}
