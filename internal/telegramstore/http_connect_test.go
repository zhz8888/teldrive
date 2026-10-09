package telegramstore

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// startSilentProxy starts a listener that accepts one connection, announces the
// accept on the returned channel, and then never answers it, so a test can pin
// how the dialer bounds or cancels the CONNECT exchange. The returned stop
// function closes the listener and releases the accepted connection.
func startSilentProxy(t *testing.T) (proxyURL *url.URL, accepted <-chan struct{}, stop func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	acceptedCh := make(chan struct{})
	release := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(acceptedCh)
		<-release
	}()
	parsed, err := url.Parse("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return parsed, acceptedCh, func() {
		close(release)
		_ = listener.Close()
	}
}

func TestHTTPConnectDialerBoundsSilentProxyExchange(t *testing.T) {
	t.Parallel()

	proxyURL, _, stop := startSilentProxy(t)
	defer stop()
	dialer, err := newHTTPConnectDialer(proxyURL, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		conn, err := dialer.DialContext(context.Background(), "tcp", "91.108.56.191:443")
		if conn != nil {
			_ = conn.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("DialContext() succeeded although the proxy never answered the CONNECT request")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DialContext() did not return although the proxy never answered the CONNECT request")
	}
}

func TestHTTPConnectDialerCancelsSilentProxyExchange(t *testing.T) {
	t.Parallel()

	proxyURL, accepted, stop := startSilentProxy(t)
	defer stop()
	// A zero dialer timeout leaves the context deadline and cancellation as the
	// only limits, so this pins that cancelling the context alone interrupts an
	// exchange that is already underway.
	dialer, err := newHTTPConnectDialer(proxyURL, 0)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		conn, err := dialer.DialContext(ctx, "tcp", "91.108.56.191:443")
		if conn != nil {
			_ = conn.Close()
		}
		done <- err
	}()
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the dialer did not reach the proxy")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DialContext() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DialContext() did not return after the context was cancelled")
	}
}

func TestHTTPConnectDialerClearsExchangeDeadlineOnEstablishedTunnel(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	const exchangeTimeout = 100 * time.Millisecond

	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()

		request, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			serverErr <- err
			return
		}
		if request.Method != http.MethodConnect {
			serverErr <- fmt.Errorf("unexpected request method %q", request.Method)
			return
		}
		if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\n\r\n"); err != nil {
			serverErr <- err
			return
		}
		// Answer only after the exchange deadline has passed: a deadline that
		// survived the handshake would fail the read below.
		time.Sleep(3 * exchangeTimeout)
		if _, err := io.WriteString(conn, "telegram"); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	proxyURL, err := url.Parse("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	dialer, err := newHTTPConnectDialer(proxyURL, exchangeTimeout)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", "91.108.56.191:443")
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	defer conn.Close()

	got := make([]byte, len("telegram"))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read tunneled bytes after the exchange deadline passed: %v", err)
	}
	if string(got) != "telegram" {
		t.Fatalf("tunneled bytes = %q, want %q", got, "telegram")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestHTTPConnectDialerIgnoresChunkedHeaderOnSuccessfulTunnel(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()

		request, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			serverErr <- err
			return
		}
		if request.Method != http.MethodConnect || request.Host != "91.108.56.191:443" {
			serverErr <- fmt.Errorf("unexpected CONNECT request: method=%q host=%q", request.Method, request.Host)
			return
		}
		if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\ntelegram"); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	proxyURL, err := url.Parse("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	dialer, err := newHTTPConnectDialer(proxyURL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := dialer.DialContext(ctx, "tcp", "91.108.56.191:443")
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	defer conn.Close()

	got := make([]byte, len("telegram"))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read tunneled bytes: %v", err)
	}
	if string(got) != "telegram" {
		t.Fatalf("tunneled bytes = %q, want %q", got, "telegram")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}
