//go:build integration

package qemutest

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"
)

func TestQMPSerializesTransactions(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	q := &QMP{conn: client, dec: json.NewDecoder(client)}

	firstRequest := make(chan string, 1)
	secondRequest := make(chan string, 1)
	releaseFirst := make(chan struct{})
	serverErr := make(chan error, 1)
	go func() {
		dec := json.NewDecoder(server)
		var first qmpRequest
		if err := dec.Decode(&first); err != nil {
			serverErr <- err
			return
		}
		firstRequest <- first.Execute

		// Write the first response from a separate goroutine while this one
		// waits to decode request two. Without QMP's transaction mutex, the
		// second request becomes visible before releaseFirst is closed.
		go func() {
			<-releaseFirst
			_, err := io.WriteString(server, `{"return":{"status":"running","running":true}}`+"\n")
			if err != nil {
				serverErr <- err
			}
		}()

		var second qmpRequest
		if err := dec.Decode(&second); err != nil {
			serverErr <- err
			return
		}
		secondRequest <- second.Execute
		if _, err := io.WriteString(server, `{"return":{}}`+"\n"); err != nil {
			serverErr <- err
		}
	}()

	statusResult := make(chan error, 1)
	go func() {
		status, err := q.QueryStatus()
		if err == nil && status != "running" {
			t.Errorf("QueryStatus = %q, want running", status)
		}
		statusResult <- err
	}()
	if got := <-firstRequest; got != "query-status" {
		t.Fatalf("first QMP request = %q, want query-status", got)
	}

	secondStarted := make(chan struct{})
	stopResult := make(chan error, 1)
	go func() {
		close(secondStarted)
		stopResult <- q.Stop()
	}()
	<-secondStarted

	select {
	case got := <-secondRequest:
		t.Fatalf("second QMP request %q was written before the first response", got)
	case err := <-serverErr:
		t.Fatalf("fake QMP server failed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseFirst)
	if err := <-statusResult; err != nil {
		t.Fatalf("QueryStatus: %v", err)
	}
	if got := <-secondRequest; got != "stop" {
		t.Fatalf("second QMP request = %q, want stop", got)
	}
	if err := <-stopResult; err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestSerialMonitorWaitForAfterIgnoresOldMarker(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	monitor := newSerialMonitor(t.Name(), nil, reader)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := io.WriteString(writer, "boot marker\n"); err != nil {
		t.Fatalf("write initial marker: %v", err)
	}
	if err := monitor.WaitFor(ctx, "boot marker", time.Second); err != nil {
		t.Fatalf("wait for initial marker: %v", err)
	}
	cursor := monitor.Cursor()

	result := make(chan error, 1)
	go func() {
		result <- monitor.WaitForAfter(ctx, cursor, "boot marker", time.Second)
	}()
	select {
	case err := <-result:
		t.Fatalf("old marker satisfied WaitForAfter: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	if _, err := io.WriteString(writer, "boot marker\n"); err != nil {
		t.Fatalf("write fresh marker: %v", err)
	}
	if err := <-result; err != nil {
		t.Fatalf("wait for fresh marker: %v", err)
	}
}
