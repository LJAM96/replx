package requestid

import (
	"strings"
	"testing"
	"time"
)

func TestShape(t *testing.T) {
	id := NewAt(time.UnixMilli(1_700_000_000_000))
	if len(id) != 36 || id[14] != '7' {
		t.Fatalf("not a v7 uuid: %s", id)
	}
	if !strings.HasPrefix(id, "018bcfe5-68") {
		t.Fatalf("timestamp prefix wrong: %s", id)
	}
	if New() == New() {
		t.Fatal("ids must be unique")
	}
}
