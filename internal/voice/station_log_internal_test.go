package voice

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestStationLogUsesUTC(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := time.Now().UTC().Truncate(time.Second)
	var output bytes.Buffer
	if err := serveStation(ctx, client, &output, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	stamp, found := strings.CutSuffix(output.String(), " Station voice stopped.\n")
	logged, err := time.Parse("2006/01/02 15:04:05", stamp)
	if !found || err != nil || logged.Before(before) || logged.After(time.Now().UTC()) {
		t.Fatalf("log line is not stamped in UTC: %q", output.String())
	}
}
