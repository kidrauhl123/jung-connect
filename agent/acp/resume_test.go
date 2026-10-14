package acp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

// fakeHandshakeServer answers initialize / session/load / session/new.
func fakeHandshakeServer(t *testing.T, wResp *io.PipeWriter, rReq *io.PipeReader, loadResult string, loadErr bool) <-chan []string {
	t.Helper()
	methods := make(chan []string, 1)
	go func() {
		var seen []string
		defer func() { methods <- seen }()
		sc := bufio.NewScanner(rReq)
		for sc.Scan() {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(sc.Bytes(), &req) != nil {
				continue
			}
			seen = append(seen, req.Method)
			switch req.Method {
			case "initialize":
				fmt.Fprintf(wResp, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}}`+"\n", req.ID)
			case "session/load":
				if loadErr {
					fmt.Fprintf(wResp, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32002,"message":"not found"}}`+"\n", req.ID)
				} else {
					fmt.Fprintf(wResp, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", req.ID, loadResult)
				}
			case "session/new":
				fmt.Fprintf(wResp, `{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"brand-new"}}`+"\n", req.ID)
				return
			}
			if req.Method == "session/load" && !loadErr {
				return
			}
		}
	}()
	return methods
}

func TestHandshake_LoadWithoutSessionIDKeepsRequestedID(t *testing.T) {
	s, wResp, rReq := newTestSession(t, nil)
	methods := fakeHandshakeServer(t, wResp, rReq, `{"modes":null}`, false)
	if err := s.handshake("old-id", ""); err != nil {
		t.Fatal(err)
	}
	got := <-methods
	if strings.Join(got, ",") != "initialize,session/load" {
		t.Fatalf("methods = %v, want no session/new", got)
	}
	if id := s.currentACPSessionID(); id != "old-id" {
		t.Fatalf("session id = %q, want old-id", id)
	}
	if s.startedNew.Load() || s.StartupWarning() != "" {
		t.Fatal("resume succeeded; must not report a new session")
	}
}

func TestHandshake_LoadErrorFallsBackAndWarnsOnce(t *testing.T) {
	s, wResp, rReq := newTestSession(t, nil)
	methods := fakeHandshakeServer(t, wResp, rReq, "", true)
	if err := s.handshake("old-id", ""); err != nil {
		t.Fatal(err)
	}
	<-methods
	if id := s.currentACPSessionID(); id != "brand-new" {
		t.Fatalf("session id = %q, want brand-new", id)
	}
	if s.StartupWarning() == "" {
		t.Fatal("expected a context-lost warning")
	}
	if s.StartupWarning() != "" {
		t.Fatal("warning must be reported only once")
	}
}
