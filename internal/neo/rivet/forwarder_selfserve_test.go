package rivet

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/fran0220/amp-proxy-neo/internal/neo/selfserve"
	"github.com/fran0220/amp-proxy-neo/pkg/config"
	"github.com/gorilla/websocket"
)

func TestSelfServeWSBootstrap(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Neo.UserID = "user_00000000000000000000000000000000_account_00000000-0000-4000-8000-000000000000_session_00000000-0000-4000-8000-000000000000"
	gw := New(cfg, nil, nil, nil, true)

	server := httptest.NewServer(http.HandlerFunc(gw.HandleWS))
	defer server.Close()

	token, err := selfserve.SignWithSecret([]byte("test-secret-32-bytes-long......."), "T-ws", "smart", cfg.Neo.UserID)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	params, _ := json.Marshal(map[string]any{"wsToken": token})
	wsURL := "ws" + server.URL[len("http"):] + "/ws"
	dialer := websocket.Dialer{Subprotocols: []string{"rivet", "rivet_conn_params." + url.QueryEscape(string(params))}}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial selfserve ws: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	var got []string
	for i := 0; i < 2; i++ {
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read startup frame %d: %v", i, err)
		}
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatalf("invalid frame json: %v", err)
		}
		got = append(got, frame["type"].(string))
	}
	if got[0] != "executor_connected" || got[1] != "agent_state" {
		t.Fatalf("startup frame types = %v", got)
	}
}
