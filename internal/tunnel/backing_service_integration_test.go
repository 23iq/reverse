package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTunnelPreservesLocalApplicationBackingConnections exercises the full
// public-listener -> tunnel -> local-app path while the app uses a separate,
// persistent TCP connection to a database-like backing service. Reverse only
// transports the application's HTTP traffic; the application's own database,
// queue, cache, and other internal connections must remain independent.
func TestTunnelPreservesLocalApplicationBackingConnections(t *testing.T) {
	databaseListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for backing service: %v", err)
	}
	defer databaseListener.Close()

	databaseDone := make(chan error, 1)
	go serveCounterDatabase(databaseListener, databaseDone)

	databaseConnection, err := net.DialTimeout("tcp", databaseListener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("connect local application to backing service: %v", err)
	}
	defer databaseConnection.Close()

	var databaseMu sync.Mutex
	databaseReader := bufio.NewReader(databaseConnection)
	localApplication := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/visits" {
			http.NotFound(writer, request)
			return
		}

		databaseMu.Lock()
		defer databaseMu.Unlock()
		if _, err := io.WriteString(databaseConnection, "INCR visits\n"); err != nil {
			http.Error(writer, "database write failed", http.StatusInternalServerError)
			return
		}
		value, err := databaseReader.ReadString('\n')
		if err != nil {
			http.Error(writer, "database read failed", http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"visits":%s}`, strings.TrimSpace(value))
	}))
	defer localApplication.Close()

	server, err := NewServer(ServerConfig{
		Verify: func(_ context.Context, request VerifyRequest) error {
			if request.Token != "database-test-token" {
				return ErrAuthenticationRejected
			}
			return nil
		},
		PublicBindHost: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer server.Close(context.Background())

	tunnelEndpoint := httptest.NewServer(server.TunnelHandler())
	defer tunnelEndpoint.Close()

	publicPort := freePort(t)
	statuses := make(chan Status, 8)
	client, err := NewClient(ClientConfig{
		ServerURL:    tunnelEndpoint.URL,
		Token:        "database-test-token",
		Domain:       "database.example.test",
		PublicPort:   publicPort,
		LocalAddress: strings.TrimPrefix(localApplication.URL, "http://"),
		OnStatus: func(status Status) {
			statuses <- status
		},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	tunnelContext, cancelTunnel := context.WithCancel(context.Background())
	tunnelResult := make(chan error, 1)
	go func() {
		tunnelResult <- client.RunOnce(tunnelContext)
	}()
	waitForStatus(t, statuses, StatusOnline)

	publicURL := "http://127.0.0.1:" + strconv.Itoa(int(publicPort))
	httpClient := &http.Client{Timeout: 5 * time.Second}
	for requestNumber, expected := range []string{`{"visits":1}`, `{"visits":2}`} {
		response, requestErr := httpClient.Get(publicURL + "/visits")
		if requestErr != nil {
			t.Fatalf("request %d through tunnel: %v", requestNumber+1, requestErr)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			t.Fatalf("read request %d response: %v", requestNumber+1, readErr)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %s, body = %q", requestNumber+1, response.Status, body)
		}
		if string(body) != expected {
			t.Fatalf("request %d body = %q, want %q", requestNumber+1, body, expected)
		}
	}

	cancelTunnel()
	select {
	case runErr := <-tunnelResult:
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			t.Fatalf("RunOnce after cancellation: %v", runErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tunnel did not stop after cancellation")
	}

	_ = databaseConnection.Close()
	select {
	case databaseErr := <-databaseDone:
		if databaseErr != nil {
			t.Fatalf("backing service: %v", databaseErr)
		}
	case <-time.After(time.Second):
		t.Fatal("backing service did not stop after its application connection closed")
	}
}

func serveCounterDatabase(listener net.Listener, done chan<- error) {
	connection, err := listener.Accept()
	if err != nil {
		done <- err
		return
	}
	defer connection.Close()

	counters := make(map[string]int)
	scanner := bufio.NewScanner(connection)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[0] != "INCR" {
			done <- fmt.Errorf("invalid database command %q", scanner.Text())
			return
		}
		counters[fields[1]]++
		if _, err := fmt.Fprintf(connection, "%d\n", counters[fields[1]]); err != nil {
			done <- err
			return
		}
	}
	done <- scanner.Err()
}
