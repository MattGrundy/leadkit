package gate

import (
	"strings"
	"testing"
)

func TestCheckPushReasons_NamesEachRefusedRefAndSkipsDeletes(t *testing.T) {
	g, _ := newTestGate(t, "github.com/org/web-client")
	const zero = "0000000000000000000000000000000000000000"
	in := "refs/heads/a " + testSHA + " refs/heads/a " + zero + "\n" +
		"refs/heads/gone " + zero + " refs/heads/gone " + testSHA + "\n"
	ok, reasons, err := g.CheckPushReasons(strings.NewReader(in), "")
	if err != nil {
		t.Fatal(err)
	}
	if ok || len(reasons) != 1 {
		t.Fatalf("ok=%v reasons=%v; want one refusal (the delete row is not judged)", ok, reasons)
	}
	if !strings.HasPrefix(reasons[0], "refs/heads/a ("+testSHA+"): ") || !strings.Contains(reasons[0], "no review record") {
		t.Errorf("the reason does not name the ref, the sha and the cause: %s", reasons[0])
	}
	// The boolean API agrees.
	if okBool, _ := g.CheckPush(strings.NewReader(in), ""); okBool != ok {
		t.Errorf("CheckPush = %v, CheckPushReasons = %v", okBool, ok)
	}
}
