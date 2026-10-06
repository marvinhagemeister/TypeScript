package ipc_test

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/microsoft/TypeScript/tsc/internal/ipc"
	"gotest.tools/v3/assert"
)

type signaledWriter struct {
	io.ReadWriteCloser
	started chan struct{}
	once    sync.Once
}

func (w *signaledWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	return w.ReadWriteCloser.Write(data)
}

func TestAsyncConnCancellationInterruptsStalledWrite(t *testing.T) {
	t.Parallel()
	client, server := net.Pipe()
	defer server.Close()
	defer client.Close()
	writer := &signaledWriter{ReadWriteCloser: client, started: make(chan struct{})}
	conn := ipc.NewAsyncConn(writer, noOpHandler{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := conn.CallWithWriteCancellation(ctx, "projectEdits", nil); done <- err }()
	<-writer.started
	// The peer never reads even the request header. A response-only deadline cannot unblock this.
	cancel()
	select {
	case err := <-done:
		assert.Assert(t, errors.Is(err, context.Canceled), "got %v", err)
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt the pipe write")
	}
}

func TestAsyncConnAlreadyCancelledEditDoesNotCloseTransport(t *testing.T) {
	t.Parallel()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	conn := ipc.NewAsyncConn(client, noOpHandler{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := conn.CallWithWriteCancellation(ctx, "projectEdits", nil)
	assert.Assert(t, errors.Is(err, context.Canceled))
	done := make(chan error, 1)
	go func() { _, writeErr := client.Write([]byte("a")); done <- writeErr }()
	assert.NilError(t, server.SetReadDeadline(time.Now().Add(time.Second)))
	var data [1]byte
	_, err = io.ReadFull(server, data[:])
	assert.NilError(t, err)
	assert.NilError(t, <-done)
}
